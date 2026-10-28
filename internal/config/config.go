// Package config 加载 JSON 配置 + 环境变量覆盖。
//
// 配置分两层（同一套字段）：
//   - config.json：进库的模板，见 config.example.json；
//   - 环境变量：容器/面板部署时覆盖单项，前缀 QWEN2API_。
//
// 设计纪律：**未知字段不报错**（JSON 解析按结构体取值），这样旧配置不会因为
// 新增字段而炸；但**非法值一定报错**（比如 timeout 写成 "abc"），不静默回默认——
// 静默回默认会让"我明明配了"变成排查噩梦。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Account 一个账号 = 一份浏览器登录态凭据。
//
// 凭据形态自动识别（见 upstream.headers）：
//   - 裸 JWT / "Bearer xxx" ⇒ 走 Authorization 头（当前网页端形态，2026-10-28 实测）；
//   - 含 "token=" 或 ";" 的完整 cookie jar ⇒ 走 Cookie 头（旧形态，双保险）。
type Account struct {
	Name  string `json:"name"`  // 展示名（面板/日志用），空则自动编号
	Token string `json:"token"` // 凭据本体；可带 cookie:/bearer: 前缀显式指定形态
	Email string `json:"email"` // 登录邮箱（配 password 时用于自动续期）
	// Password 明文登录密码。配了它，网关在凭据失效时会**自动重登**换新 token
	// （signin 签发的 token 是 cookie 形态，且会过期）。
	// ⚠️ 与 token 二选一即可：只配 password 也行，启动时自动登录。
	Password string `json:"password"`
	Note     string `json:"note"` // 可选备注
}

// Upstream 上游站点参数。默认值 = 2026-10-28 实测抓包。
type Upstream struct {
	BaseURL     string `json:"base_url"`      // https://chat.qwen.ai
	AuthBaseURL string `json:"auth_base_url"` // https://auth.qwen.ai（登录态刷新域，与业务域分离）
	// EntitlementBaseURL 积分（Credits）域。
	//
	// 🔴 它是**第三个域名**：余额/消费明细既不在 chat.qwen.ai 也不在 auth.qwen.ai，
	// 而在 sg-entitlement.qwen.com。2026-10-29 实测：用量与账单页的余额走这里。
	EntitlementBaseURL string `json:"entitlement_base_url"`
	Version            string `json:"version"`    // version 请求头；0.3.26 为当前前端版本
	UserAgent          string `json:"user_agent"` // Chrome UA，需与 sec-ch-ua 版本自洽
	// BxUA / BxUmidtoken 是阿里的设备指纹头。实测前端自己也会带
	// "defaultFY2_load_failed with timeout"，说明**不是硬门槛**；留空即不发。
	BxUA        string `json:"bx_ua"`
	BxUmidtoken string `json:"bx_umidtoken"`
	TimeoutS    int    `json:"timeout_s"`         // 单请求总超时（含流式读取），默认 180
	ConnectS    int    `json:"connect_timeout_s"` // 建连超时，默认 20
	// InsecureSkipVerify 仅供抓包调试（自签代理），生产别开。
	InsecureSkipVerify bool `json:"insecure_skip_verify"`
}

// Video 文生视频默认参数（写进 extra.meta.biz_data）。
//
// 合法档位（来自上游 biz_data 的取值域，2026-10-28 实测 + credits/pricing）：
//   - Resolution：720P / 1080P（1080P 走付费积分，720P 走每日免费额度）
//   - Duration：5 / 10 / 15 秒
//
// 上游对每个 (resolution, duration) 组合有独立计价（720P 5s=40 积分起），
// 网关不做档位换算——写多少发多少，超出档位由上游报错。
type Video struct {
	Resolution string `json:"resolution"`
	Duration   int    `json:"duration"`
	Audio      bool   `json:"audio"`
	// RootModel 生成内核（实测 happyhorse11）。
	RootModel string `json:"root_model"`
	// Size 默认画幅。
	Size string `json:"size"`
	// Pricing 积分单价表：resolution → 秒数 → 积分。
	//
	// 例如 {"720P":{"5":40,"10":80,"15":120}, "1080P":{"5":70}}。
	// 空表 ⇒ 不启用积分模式，退回 MaxVideoPerDay 的个数口径。
	// 表里查不到的组合按"最贵档"保守估计，宁可少发也不透支。
	Pricing map[string]map[string]int `json:"pricing"`
}

// VideoCredits 查一次视频的积分单价。
//
// 查不到时返回 ok=false —— 调用方要么按"最贵档"保守处理，要么直接拒绝。
// 绝不默认 0：那等于放任免费生成。
func (v Video) VideoCredits(resolution string, durationS int) (int, bool) {
	if len(v.Pricing) == 0 {
		return 0, false
	}
	durs, ok := v.Pricing[resolution]
	if !ok {
		return 0, false
	}
	if c, ok := durs[strconv.Itoa(durationS)]; ok {
		return c, true
	}
	return 0, false
}

// MaxVideoCredits 价格表里最贵的单价（用于查不到时的保守兜底）。
func (v Video) MaxVideoCredits() int {
	max := 0
	for _, durs := range v.Pricing {
		for _, c := range durs {
			if c > max {
				max = c
			}
		}
	}
	return max
}

// Pool 账号池策略。
type Pool struct {
	// MaxRotate 单次请求最多换号次数（401/额度耗尽触发），默认 3。
	MaxRotate int `json:"max_rotate"`
	// CooldownSoftS 限流/风控软冷却基数（秒），默认 600，连续触发指数退避。
	CooldownSoftS int `json:"cooldown_soft_s"`
	// CooldownAuthS 凭据失效（401）冷却（秒），默认 1800。
	CooldownAuthS int `json:"cooldown_auth_s"`
	// MaxInflightPerAccount 单账号并发在途上限，默认 2。
	MaxInflightPerAccount int `json:"max_inflight_per_account"`
	// MaxVideoPerDay 单账号**每日视频任务**个数上限，0 = 不限，默认 1。
	// 这是**兜底口径**：上游真正的计费单位是积分（见 Video.Pricing）。
	// 拿到价格表后改用 MaxVideoCreditsPerDay，这条就退化成"最多几个"的硬上限。
	MaxVideoPerDay int `json:"max_video_per_day"`
	// MaxVideoCreditsPerDay 单账号**每日视频积分预算**，0 = 不启用积分模式。
	//
	// 🔴 视频按积分计费，不是一个价：720P 5s 与 1080P 15s 差好几倍。
	// 只有同时给了 Video.Pricing（单价表）才生效；否则退回 MaxVideoPerDay 的个数口径。
	MaxVideoCreditsPerDay int `json:"max_video_credits_per_day"`
	// MaxImagePerDay 单账号**每日成功出图**张数上限，0 = 不限，默认 6。
	//
	// 为什么设 6 而不是上游实测的 9：留 3 张缓冲。打到硬限后上游不给明确错误，
	// 而是"只发心跳、永不产出"（实测要等 180s 才知道失败），体验很差。
	// 只统计**成功出图**：实测 9 张成功即耗尽，中途的挂起/失败并不扣额度。
	MaxImagePerDay int `json:"max_image_per_day"`
}

// Config 顶层配置。
type Config struct {
	Listen       string   `json:"listen"`        // ":8400"
	APIKeys      []string `json:"api_keys"`      // 空 = 不鉴权（仅限内网/本机）
	DefaultModel string   `json:"default_model"` // qwen3.7-plus
	// DefaultThinking 请求未指定时的思考档：auto / thinking / fast。
	// auto = 前端默认（自动判断）；thinking = 强制思考；fast = 关闭思考。
	DefaultThinking string `json:"default_thinking"`
	// ExposeReasoning 是否把上游 thinking_summary 透出为 reasoning_content，默认 true。
	ExposeReasoning *bool `json:"expose_reasoning"`
	// NewChatPerRequest 每请求新建会话（无状态，默认 true）。
	// false 时按会话键复用 chat_id（省一次上游调用，但多轮语义依赖客户端回传）。
	NewChatPerRequest *bool `json:"new_chat_per_request"`
	// StateFile 池状态落盘路径（空 = 不落盘）；重启后保留冷却/失败计数。
	StateFile string `json:"state_file"`
	// ImageModel 文生图用的**图片模型名**（写进 extra.meta.model）。
	// 🔴 不传它时上游只是"聊图"，不保证出图（2026-10-28 实测）。
	ImageModel string `json:"image_model"`
	Video      Video  `json:"video"`

	Upstream Upstream  `json:"upstream"`
	Pool     Pool      `json:"pool"`
	Accounts []Account `json:"accounts"`
}

// Default 返回一份「开箱能跑」的默认配置。
func Default() Config {
	return Config{
		Listen:          ":8400",
		DefaultModel:    "qwen3.7-plus",
		DefaultThinking: "auto",
		StateFile:       "./data/state.json",
		ImageModel:      "qwen-image-3.0",
		Video: Video{
			// 积分单价：来自官方计价接口 `GET /api/v2/credits/pricing`（2026-10-29 实测）。
			//
			// 🔴 价格**按模型版本分两套**，别拿错表：
			//   hh11*（HappyHorse 1.1，本仓 root_model 默认值 happyhorse11）
			//     720P 30/60/90  1080P 40/80/120
			//   hh*（HappyHorse 1.0）
			//     720P 40/80/120 1080P 70/140/210
			// 下面这张是 hh11 的。账单实测 `HappyHorse1.1.720P.5s = -30`、`1080P.15s = -120`
			// 与之一致；换 root_model 时价格表要跟着换，否则预算会算错 33%~75%。
			// 🔴 别用「个数」想象视频成本：720P 5s 与 1080P 15s 差 4 倍。
			Pricing: map[string]map[string]int{
				"720P":  {"5": 30, "10": 60, "15": 90},
				"1080P": {"5": 40, "10": 80, "15": 120},
			},
			Resolution: "720P",
			Duration:   5,
			Audio:      true,
			RootModel:  "happyhorse11",
			Size:       "16:9",
		},
		Upstream: Upstream{
			BaseURL:            "https://chat.qwen.ai",
			AuthBaseURL:        "https://auth.qwen.ai",
			EntitlementBaseURL: "https://sg-entitlement.qwen.com",
			Version:            "0.3.26",
			UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
				"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36",
			TimeoutS: 180,
			ConnectS: 20,
		},
		Pool: Pool{
			MaxRotate:             3,
			CooldownSoftS:         600,
			CooldownAuthS:         1800,
			MaxInflightPerAccount: 2,
			MaxImagePerDay:        6,
			MaxVideoPerDay:        1,
			// 默认 0 = 不启用。理由：现在能读**账号真实余额**了（见 upstream.Credits），
			// 余额才是唯一真口径；再叠一层「40 分/天」会把会员号的 270 分锁死。
			// 想额外设上限就填正数（按账号、按自然日）。
			MaxVideoCreditsPerDay: 0,
		},
	}
}

// Load 读配置文件（path 可为空 = 只用环境变量）+ 环境变量覆盖 + 校验。
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("读配置 %s: %w", path, err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv 环境变量覆盖（QWEN2API_*）。
//
// 🔴 账号列表用 QWEN2API_ACCOUNTS 整体**替换**配置文件里的列表，而不是追加：
// 追加会让"我以为清空了、其实旧号还在"，容器场景这是安全事故。
func applyEnv(cfg *Config) {
	if v := os.Getenv("QWEN2API_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("QWEN2API_API_KEYS"); v != "" {
		cfg.APIKeys = splitList(v)
	}
	if v := os.Getenv("QWEN2API_ACCOUNTS"); v != "" {
		var list []Account
		for i, tok := range splitList(v) {
			list = append(list, Account{Name: fmt.Sprintf("env-%d", i+1), Token: tok})
		}
		cfg.Accounts = list
	}
	if v := os.Getenv("QWEN2API_DEFAULT_MODEL"); v != "" {
		cfg.DefaultModel = v
	}
	if v := os.Getenv("QWEN2API_DEFAULT_THINKING"); v != "" {
		cfg.DefaultThinking = v
	}
	if v := os.Getenv("QWEN2API_STATE_FILE"); v != "" {
		cfg.StateFile = v
	}
	if v := os.Getenv("QWEN2API_BASE_URL"); v != "" {
		cfg.Upstream.BaseURL = v
	}
	if v := os.Getenv("QWEN2API_VERSION"); v != "" {
		cfg.Upstream.Version = v
	}
	if v := os.Getenv("QWEN2API_USER_AGENT"); v != "" {
		cfg.Upstream.UserAgent = v
	}
	if v := os.Getenv("QWEN2API_TIMEOUT_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Upstream.TimeoutS = n
		}
	}
}

// normalize 补默认值 + 校验非法值。
func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = ":8400"
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "qwen3.7-plus"
	}
	if c.DefaultThinking == "" {
		c.DefaultThinking = "auto"
	}
	if _, ok := thinkingGears[c.DefaultThinking]; !ok {
		return fmt.Errorf("default_thinking=%q 非法，可选 auto/thinking/fast", c.DefaultThinking)
	}
	if c.ImageModel == "" {
		c.ImageModel = "qwen-image-3.0"
	}
	if c.Video.Resolution == "" {
		c.Video.Resolution = "720P"
	}
	if c.Video.Duration <= 0 {
		c.Video.Duration = 5
	}
	if c.Video.RootModel == "" {
		c.Video.RootModel = "happyhorse11"
	}
	if c.Video.Size == "" {
		c.Video.Size = "16:9"
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://chat.qwen.ai"
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")
	if c.Upstream.AuthBaseURL == "" {
		c.Upstream.AuthBaseURL = "https://auth.qwen.ai"
	}
	c.Upstream.AuthBaseURL = strings.TrimRight(c.Upstream.AuthBaseURL, "/")
	if c.Upstream.Version == "" {
		c.Upstream.Version = "0.3.26"
	}
	if c.Upstream.UserAgent == "" {
		c.Upstream.UserAgent = Default().Upstream.UserAgent
	}
	if c.Upstream.TimeoutS <= 0 {
		c.Upstream.TimeoutS = 180
	}
	if c.Upstream.ConnectS <= 0 {
		c.Upstream.ConnectS = 20
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = 3
	}
	if c.Pool.CooldownSoftS <= 0 {
		c.Pool.CooldownSoftS = 600
	}
	if c.Pool.CooldownAuthS <= 0 {
		c.Pool.CooldownAuthS = 1800
	}
	if c.Pool.MaxInflightPerAccount <= 0 {
		c.Pool.MaxInflightPerAccount = 2
	}
	// 账号去重 + 过滤空 token（空 token 会让池里出现"永远失败"的僵尸号）。
	seen := map[string]bool{}
	out := c.Accounts[:0]
	for i, a := range c.Accounts {
		a.Token = strings.TrimSpace(a.Token)
		a.Email = strings.TrimSpace(a.Email)
		// 只有密码也能用（启动时自动登录）；两者都没有才是空账号。
		if a.Token == "" && a.Password == "" {
			continue
		}
		key := a.Token
		if key == "" {
			key = "pw:" + a.Email
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if a.Name == "" {
			a.Name = fmt.Sprintf("acct-%d", i+1)
		}
		out = append(out, a)
	}
	c.Accounts = out
	return nil
}

// Timeout 解析成 time.Duration。
func (u Upstream) Timeout() time.Duration { return time.Duration(u.TimeoutS) * time.Second }

// ConnectTimeout 建连超时。
func (u Upstream) ConnectTimeout() time.Duration {
	return time.Duration(u.ConnectS) * time.Second
}

// ReasoningEnabled 是否透出思考内容（默认 true）。
func (c Config) ReasoningEnabled() bool {
	return c.ExposeReasoning == nil || *c.ExposeReasoning
}

// NewChatEach 是否每请求新建会话（默认 true）。
func (c Config) NewChatEach() bool {
	return c.NewChatPerRequest == nil || *c.NewChatPerRequest
}

// thinkingGears 思考档位表 —— 与前端 UI 三档逐字对齐（2026-10-28 抓包复核，
// 与 2026-09-24 一致）。
var thinkingGears = map[string]string{
	"auto":     "auto",
	"thinking": "thinking",
	"fast":     "fast",
}

// Gear 归一化思考档位；未知值回落 auto。
func Gear(name string) string {
	if _, ok := thinkingGears[name]; ok {
		return name
	}
	return "auto"
}

func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == ';' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}
