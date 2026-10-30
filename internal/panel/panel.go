// Package panel 是内嵌的单页管理面板（零外部依赖，纯静态 + JSON 接口）。
//
// 它只读运行态（账号池 / 请求流水 / 配置摘要）并支持两个写操作：
// 重置账号状态、在线试跑。所有写操作都需要与前门同一把 API Key。
package panel

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

//go:embed index.html
var indexHTML string

// Backend 面板需要的能力（由 server.Server 实现）。
//
// 写操作（导入/删除/重登）都在 Backend 上而不是面板里直接改池：
// 落盘、登录校验、日志都属服务端职责，面板只做编排与回显。
type Backend interface {
	Pool() *pool.Pool
	Log() *reqlog.Log
	Upstream() *upstream.Client
	Config() config.Config
	Version() string
	UptimeSeconds() int

	// ImportAccounts 批量导入（validate=true 时会逐个登录校验）。
	ImportAccounts(ctx context.Context, in []config.Account, validate bool) ([]pool.AddResult, error)
	// DeleteAccount 移除账号并落盘。
	DeleteAccount(name string) (bool, error)
	// TestAccount 用保存的密码重登一次。
	TestAccount(ctx context.Context, name string) error
	// Models 模型清单（复用前门缓存）。
	Models(ctx context.Context) any
	// RefreshCredits 刷新积分余额（force=true 无视 TTL）。
	RefreshCredits(ctx context.Context, force bool)
}

// Panel 面板 handler。
type Panel struct {
	b Backend
}

// New 构造面板。
func New(b Backend) *Panel { return &Panel{b: b} }

// ServeHTTP 路由：/panel/ 静态页，/panel/api/* JSON。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		if strings.HasPrefix(r.URL.Path, "/panel/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// 静态页仍然给（页面里会弹登录框），只是拿不到数据。
	}
	switch {
	case r.URL.Path == "/panel" || r.URL.Path == "/panel/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(indexHTML))
	case r.URL.Path == "/panel/api/overview":
		if !p.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		p.overview(w, r)
	case r.URL.Path == "/panel/api/credits/refresh" && r.Method == http.MethodPost:
		p.mustAuth(w, r, func(w http.ResponseWriter, r *http.Request) {
			p.b.RefreshCredits(r.Context(), true)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		})
	case r.URL.Path == "/panel/api/models":
		if !p.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": p.b.Models(r.Context())})
	case r.URL.Path == "/panel/api/accounts/import" && r.Method == http.MethodPost:
		p.mustAuth(w, r, p.importAccounts)
	case r.URL.Path == "/panel/api/accounts/delete" && r.Method == http.MethodPost:
		p.mustAuth(w, r, p.deleteAccount)
	case r.URL.Path == "/panel/api/accounts/test" && r.Method == http.MethodPost:
		p.mustAuth(w, r, p.testAccount)
	case r.URL.Path == "/panel/api/reset" && r.Method == http.MethodPost:
		if !p.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		name := r.URL.Query().Get("name")
		if name == "" || !p.b.Pool().Reset(name) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

// mustAuth 统一鉴权包装（写操作一律要求同一把 Key）。
func (p *Panel) mustAuth(w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if !p.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	h(w, r)
}

// importAccounts 批量导入账号。
//
// 请求：{"accounts":[{"name":"","email":"","password":"","note":""}],"validate":true}
// 响应：{"ok":true,"total":n,"imported":k,"skipped":m,"results":[{name,email,ok,error}]}
func (p *Panel) importAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Accounts []config.Account `json:"accounts"`
		Validate *bool            `json:"validate"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if len(body.Accounts) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "accounts 为空"})
		return
	}
	if len(body.Accounts) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "单次最多 500 个（分批导入）"})
		return
	}
	// 默认校验：不校验等于往池里塞哑弹（错密码的号会被选中、失败、然后冷却）。
	validate := true
	if body.Validate != nil {
		validate = *body.Validate
	}

	results, persistErr := p.b.ImportAccounts(r.Context(), body.Accounts, validate)
	imported, skipped := 0, 0
	for _, it := range results {
		if it.OK {
			imported++
		} else {
			skipped++
		}
	}
	resp := map[string]any{
		"ok": imported > 0, "total": len(results),
		"imported": imported, "skipped": skipped, "results": results,
	}
	if persistErr != nil {
		resp["ok"] = false
		resp["persist_error"] = persistErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (p *Panel) deleteAccount(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	ok, err := p.b.DeleteAccount(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) testAccount(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := p.b.TestAccount(r.Context(), name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	// 顺带刷余额（2 分钟 TTL）：面板 5s 轮询，靠 TTL 挡住不会打热三方域名；
	// 没有这一步，首次打开面板会看到一片"余额未知"，像功能没做完。
	p.b.RefreshCredits(r.Context(), false)

	cfg := p.b.Config()
	// 只暴露"配置摘要"，不回显 token / api key。
	summary := map[string]any{
		"listen":           cfg.Listen,
		"default_model":    cfg.DefaultModel,
		"default_thinking": cfg.DefaultThinking,
		"api_key_required": len(cfg.APIKeys) > 0,
		"upstream": map[string]any{
			"base_url":  cfg.Upstream.BaseURL,
			"version":   cfg.Upstream.Version,
			"timeout_s": cfg.Upstream.TimeoutS,
		},
		"pool": map[string]any{
			"max_rotate":                cfg.Pool.MaxRotate,
			"cooldown_soft_s":           cfg.Pool.CooldownSoftS,
			"cooldown_auth_s":           cfg.Pool.CooldownAuthS,
			"max_inflight_per_account":  cfg.Pool.MaxInflightPerAccount,
			"max_image_per_day":         cfg.Pool.MaxImagePerDay,
			"max_video_per_day":         cfg.Pool.MaxVideoPerDay,
			"max_video_credits_per_day": cfg.Pool.MaxVideoCreditsPerDay,
		},
		"video": map[string]any{
			"root_model": cfg.Video.RootModel,
			"resolution": cfg.Video.Resolution,
			"duration":   cfg.Video.Duration,
			"size":       cfg.Video.Size,
			"pricing":    cfg.Video.Pricing,
		},
		"entitlement_base_url": cfg.Upstream.EntitlementBaseURL,
		"state_file":           cfg.StateFile,
		"reasoning":            cfg.ReasoningEnabled(),
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "qwen2api",
		"version":  p.b.Version(),
		"uptime_s": p.b.UptimeSeconds(),
		"accounts": p.b.Pool().Snapshot(),
		"stats":    p.b.Log().Stats(),
		"requests": p.b.Log().Recent(80),
		"config":   summary,
	})
}

// authorized 与 server 前门同口径：api_keys 为空则不鉴权。
func (p *Panel) authorized(r *http.Request) bool {
	keys := p.b.Config().APIKeys
	if len(keys) == 0 {
		return true
	}
	token := ""
	if v := r.Header.Get("Authorization"); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			token = strings.TrimSpace(v[7:])
		} else {
			token = strings.TrimSpace(v)
		}
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
