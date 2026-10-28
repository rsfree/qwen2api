// Package panel 是内嵌的单页管理面板（零外部依赖，纯静态 + JSON 接口）。
//
// 它只读运行态（账号池 / 请求流水 / 配置摘要）并支持两个写操作：
// 重置账号状态、在线试跑。所有写操作都需要与前门同一把 API Key。
package panel

import (
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
type Backend interface {
	Pool() *pool.Pool
	Log() *reqlog.Log
	Upstream() *upstream.Client
	Config() config.Config
	Version() string
	UptimeSeconds() int
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

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
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
			"max_rotate":               cfg.Pool.MaxRotate,
			"cooldown_soft_s":          cfg.Pool.CooldownSoftS,
			"cooldown_auth_s":          cfg.Pool.CooldownAuthS,
			"max_inflight_per_account": cfg.Pool.MaxInflightPerAccount,
		},
		"reasoning": cfg.ReasoningEnabled(),
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
