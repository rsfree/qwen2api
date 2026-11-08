package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Field 面板「配置」页的一个字段。
//
// 只在这里登记的字段才允许从面板写回 —— 白名单而不是黑名单：
// 配置里有 accounts / api_keys / panel.session_secret 这类"写错就把服务锁死"的项，
// 黑名单漏一个就是线上事故。
type Field struct {
	// Key 点分路径，如 `pool.max_rotate`。
	Key   string `json:"key"`
	Label string `json:"label"`
	// Type int | bool | string | enum | kv
	//
	// kv 是 map[string]string（如 video.aliases）：面板渲染成「每行 key=value」，
	// 提交时仍是 JSON 对象。为什么不用 string 存 JSON 文本：那会让「写错一个引号」
	// 变成一次启动失败，而 kv 的解析错误能在保存时就指到具体哪一行。
	Type string `json:"type"`
	// Min / Max 仅 int 有效（闭区间）。
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
	// Enum 仅 enum 有效。
	Enum []string `json:"enum,omitempty"`
	// Hot 改完是否**立即生效**（false = 必须重启，面板会明确标出来）。
	Hot bool `json:"hot"`
	// Hint 给用户的一句说明（为什么有这个开关 / 改了的后果）。
	Hint string `json:"hint,omitempty"`
}

// Fields 面板可编辑字段（顺序即表单顺序）。
var Fields = []Field{
	{Key: "default_model", Label: "默认模型", Type: "string", Hot: true,
		Hint: "请求没带 model 时用它"},
	{Key: "default_thinking", Label: "默认思考档", Type: "enum",
		Enum: []string{"auto", "thinking", "fast"}, Hot: true,
		Hint: "auto = 前端默认；thinking = 强制深度思考；fast = 关闭"},
	{Key: "expose_reasoning", Label: "透出 reasoning_content", Type: "bool", Hot: true,
		Hint: "把上游的思考摘要放进响应，OpenAI SDK 可直接读"},
	{Key: "image_model", Label: "图片内核模型", Type: "string", Hot: true,
		Hint: "写进 extra.meta.model；空 = 上游默认"},
	{Key: "image_timeout_s", Label: "生图单次超时（秒）", Type: "int", Min: 30, Max: 900, Hot: true,
		Hint: "单次上游尝试的预算。实测出图 60s、最慢 137s；超时会换号再试一次（最坏 2×）"},
	{Key: "blob_base_url", Label: "图片托管公网基址", Type: "string", Hot: true,
		Hint: "data URI/base64 图片会落到 {它}/v1/blobs/<id> 由上游来拉；必须是公网域名"},

	{Key: "upstream.egress_ip_url", Label: "出口 IP 探测源", Type: "string", Hot: true,
		Hint: `纯文本回 IP 的地址（默认 https://api.ipify.org）；填 "-" 关闭探测`},
	{Key: "upstream.timeout_s", Label: "上游超时（秒）", Type: "int", Min: 10, Max: 900, Hot: false,
		Hint: "改完需重启（HTTP 客户端在启动时建好）"},

	{Key: "pool.max_rotate", Label: "单请求最多换几个号", Type: "int", Min: 1, Max: 10, Hot: true,
		Hint: "失败换号的轮换上限；3 已能覆盖绝大多数抖动"},
	{Key: "pool.cooldown_soft_s", Label: "软冷却（秒）", Type: "int", Min: 30, Max: 86400, Hot: true,
		Hint: "限流/风控/抖动后的冷却时长"},
	{Key: "pool.cooldown_auth_s", Label: "凭据失效冷却（秒）", Type: "int", Min: 60, Max: 86400, Hot: true,
		Hint: "401/403 后的冷却时长；太短会反复撞坏号"},
	{Key: "pool.max_inflight_per_account", Label: "单号并发上限", Type: "int", Min: 1, Max: 8, Hot: true,
		Hint: "同一账号同时在途的请求数"},
	{Key: "pool.max_image_per_day", Label: "单号每日出图上限", Type: "int", Min: 0, Max: 50, Hot: true,
		Hint: "0 = 不限。打到上游硬限不会报错、只会静默挂起，所以本地先拦"},
	{Key: "pool.max_video_per_day", Label: "单号每日视频任务数", Type: "int", Min: 0, Max: 50, Hot: true,
		Hint: "兜底口径（未配积分单价表时生效）"},
	{Key: "pool.max_video_credits_per_day", Label: "单号每日视频积分预算", Type: "int", Min: 0, Max: 100000, Hot: true,
		Hint: "0 = 不启用；配了 video.pricing 才按积分算"},
	{Key: "pool.warm_chats_per_account", Label: "每号预建会话数", Type: "int", Min: 0, Max: 5, Hot: true,
		Hint: "0 = 关闭。实测净省约 1.2 秒/次（建会话 3.2s，命中后提交阶段会多 ~2s）"},
	{Key: "pool.chat_pool_ttl_s", Label: "预建会话有效期（秒）", Type: "int", Min: 60, Max: 86400, Hot: true,
		Hint: "超过就丢弃，避免用很旧的空会话去提交"},

	// 预热节奏：流量变了就调这里，不用发版。改完下一轮 tick 生效。
	{Key: "pool.keepalive.tick_s", Label: "预热轮询间隔（秒）", Type: "int", Min: 5, Max: 300, Hot: true,
		Hint: "每轮做一次 token 续期 / 会话预建 / 连接保活。调小跟得上突发，但上游调用更密"},
	{Key: "pool.keepalive.token_window_s", Label: "token 提前续期窗口（秒）", Type: "int", Min: 60, Max: 1800, Hot: true,
		Hint: "access 剩余寿命小于它就开始换新；必须大于轮询间隔，否则热完就过期"},
	{Key: "pool.keepalive.conc_factor", Label: "预热并发系数", Type: "int", Min: 1, Max: 10, Hot: true,
		Hint: "会话/连接的覆盖范围 = 并发峰值 × 它 + 4。调大更抗突发，代价是更多空会话与保活流量"},
	{Key: "pool.keepalive.adopt_batch", Label: "每轮补 refresh_token 数", Type: "int", Min: 1, Max: 100, Hot: true,
		Hint: "补 refresh 要密码登录（最易吃风控），故意摊慢；冷号多时可以调大"},
	{Key: "pool.keepalive.parallel", Label: "预热并发数", Type: "int", Min: 1, Max: 32, Hot: true,
		Hint: "单批次并行度。调大热得更快，但同一时刻打上游更密"},
	{Key: "pool.keepalive.batch_max", Label: "单轮预热上限", Type: "int", Min: 4, Max: 512, Hot: true,
		Hint: "一轮最多处理几个号。太大容易把上游打热"},
	{Key: "pool.keepalive.target_max", Label: "预热目标上限", Type: "int", Min: 8, Max: 4096, Hot: true,
		Hint: "按流量估算出的目标数上限。热满时间必须小于 token 的 15 分钟 TTL"},

	{Key: "video.root_model", Label: "视频内核模型", Type: "string", Hot: true,
		Hint: "实测 happyhorse11；改错会 Bad_Request"},
	{Key: "video.resolution", Label: "视频默认分辨率", Type: "enum",
		Enum: []string{"720P", "1080P"}, Hot: true,
		Hint: "请求没带 resolution 时用它；1080P 走付费积分"},
	{Key: "video.duration", Label: "视频默认时长（秒）", Type: "int", Min: 5, Max: 15, Hot: true,
		Hint: "请求没带 duration 时用它；上游只支持 5 / 10 / 15"},
	{Key: "video.size", Label: "视频默认画幅", Type: "string", Hot: true,
		Hint: "如 16:9 / 1:1 / 9:16；空 = 上游默认"},
	{Key: "video.aliases", Label: "模型别名", Type: "kv", Hot: true,
		Enum: []string{"happyhorse11", "happyhorse", "wan27"},
		Hint: "客户端传的模型名 → 生成内核，每行一条 `名字=内核`。" +
			"只是路由标签：产物由本网关上游生成，与 doubao 无关。留空 = 不启用"},
}

// fieldByKey 查字段定义。
func fieldByKey(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Normalize 按字段类型校验并归一化一个值（JSON 解出来的 any）。
func (f Field) Normalize(v any) (any, error) {
	switch f.Type {
	case "int":
		var n int
		switch x := v.(type) {
		case float64:
			n = int(x)
			if float64(n) != x {
				return nil, fmt.Errorf("%s 必须是整数", f.Label)
			}
		case int:
			n = x
		case json.Number:
			i, err := x.Int64()
			if err != nil {
				return nil, fmt.Errorf("%s 必须是整数", f.Label)
			}
			n = int(i)
		default:
			return nil, fmt.Errorf("%s 必须是整数", f.Label)
		}
		if n < f.Min || n > f.Max {
			return nil, fmt.Errorf("%s 超出范围 %d~%d（收到 %d）", f.Label, f.Min, f.Max, n)
		}
		return n, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s 必须是 true/false", f.Label)
		}
		return b, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s 必须是字符串", f.Label)
		}
		for _, e := range f.Enum {
			if e == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s 只接受 %s（收到 %q）", f.Label, strings.Join(f.Enum, " / "), s)
	case "kv":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s 必须是对象（每行 key=value）", f.Label)
		}
		// 防呆：别名是「几个」的量级，200 条必然是脚本灌错了。
		if len(m) > 200 {
			return nil, fmt.Errorf("%s 最多 200 条（收到 %d 条）", f.Label, len(m))
		}
		out := make(map[string]string, len(m))
		for k, val := range m {
			key := strings.TrimSpace(k)
			if key == "" {
				return nil, fmt.Errorf("%s 有空键", f.Label)
			}
			str, isStr := val.(string)
			if !isStr {
				return nil, fmt.Errorf("%s 的 %q 值必须是字符串", f.Label, key)
			}
			str = strings.TrimSpace(str)
			if str == "" {
				return nil, fmt.Errorf("%s 的 %q 没有值", f.Label, key)
			}
			if len(f.Enum) > 0 {
				okEnum := false
				for _, e := range f.Enum {
					if e == str {
						okEnum = true
						break
					}
				}
				if !okEnum {
					return nil, fmt.Errorf("%s 的 %q 只接受 %s（收到 %q）",
						f.Label, key, strings.Join(f.Enum, " / "), str)
				}
			}
			out[key] = str
		}
		return out, nil
	default: // string
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s 必须是字符串", f.Label)
		}
		return strings.TrimSpace(s), nil
	}
}

// Apply 把面板提交的 patch 合并进 raw（原始 config.json 的 map）。
//
// 只处理 Fields 里登记过的键：
//   - 字段存在 → 校验后写入（点分路径会逐层建子 map）
//   - 字段不存在 → 返回错误（拒绝写未知键，避免面板把配置写坏）
//
// 返回 applied（值真的变了的键）与 needRestart（变了但不支持热更的键）。
func Apply(raw map[string]any, patch map[string]any) (applied, needRestart []string, err error) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 稳定输出，便于测试与前端展示

	for _, key := range keys {
		f, ok := fieldByKey(key)
		if !ok {
			return nil, nil, fmt.Errorf("不支持修改 %q（只能改面板列出的字段）", key)
		}
		val, err := f.Normalize(patch[key])
		if err != nil {
			return nil, nil, err
		}
		old, had := getPath(raw, key)
		if had && equalAny(old, val) {
			continue // 没变就别动文件（避免无意义的重启标记）
		}
		setPath(raw, key, val)
		applied = append(applied, key)
		if !f.Hot {
			needRestart = append(needRestart, key)
		}
	}
	return applied, needRestart, nil
}

// getPath 读取点分路径。
func getPath(m map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	cur := any(m)
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath 写入点分路径（中间层不存在则建 map）。
func setPath(m map[string]any, key string, val any) {
	parts := strings.Split(key, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = val
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// equalAny 宽松比较（JSON 数字解出来都是 float64，int 与 float64 要能相等）。
func equalAny(a, b any) bool {
	fa, aok := toFloat(a)
	fb, bok := toFloat(b)
	if aok && bok {
		return fa == fb
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// FieldValues 从当前配置里取每个字段的值（供面板渲染表单）。
func FieldValues(c Config) map[string]any {
	out := make(map[string]any, len(Fields))
	b, err := json.Marshal(c)
	if err != nil {
		return out
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return out
	}
	for _, f := range Fields {
		if v, ok := getPath(raw, f.Key); ok && v != nil {
			out[f.Key] = v
			continue
		}
		// 缺值有两种原因，都不能让表单显示成空白：
		//   · 字段带 omitempty（如 blob_base_url），空值被 json 省掉 ⇒ 用零值；
		//   · 指针类开关（expose_reasoning 的 nil = 默认 true）⇒ 按"实际生效值"填。
		switch f.Key {
		case "expose_reasoning":
			out[f.Key] = c.ReasoningEnabled()
		default:
			switch f.Type {
			case "int":
				out[f.Key] = 0
			case "bool":
				out[f.Key] = false
			case "kv":
				// 空表给空对象而不是空串：前端按「键值对」渲染，类型不一致会画错。
				out[f.Key] = map[string]any{}
			default:
				out[f.Key] = ""
			}
		}
	}
	return out
}
