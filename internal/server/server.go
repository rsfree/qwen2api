// Package server 是 OpenAI 兼容前门：/v1/chat/completions、/v1/messages、
// /v1/responses、/v1/models，外加运维面 /healthz /readyz /stats 与 /panel/。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/runlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// Server HTTP 前门。
type Server struct {
	cfg     config.Config
	pool    *pool.Pool
	up      *upstream.Client
	models  *ModelCache
	log     *reqlog.Log
	started time.Time
	version string
	panel   http.Handler
	// videos 视频任务 → 账号映射（内存 2h）。
	videos *videoStore
	// files 上传条目的暂存（内存 2h）。
	files *FileStore
	// blobs 图片外链托管（内存 2h）—— 给上游一个自己能拉的 URL，绕开 STS。
	blobs *BlobStore
	// 积分余额读取失败的日志去重（见 credits.go）。
	creditErrMu sync.Mutex
	creditErrAt time.Time
	// accountsMu 串行化 accounts.json 的写入（面板导入/删除）。
	accountsMu sync.Mutex
	// creditCursor 余额刷新轮转游标（受 creditRefreshMu 保护）。
	creditCursor int
	// refreshErrAt 预热续期的失败日志去重（账号名 → 上次记录时间）。
	refreshErrAt map[string]time.Time
	// keysMu 保护前门令牌（config.api_keys 或面板托管的 apikeys.json）。
	// 每次 /v1 请求都要读它，所以读写必须轻（RLock + 切片拷贝）。
	keysMu      sync.RWMutex
	keys        []config.APIKey
	keysPath    string
	keysManaged bool
	keysInit    bool
	// runLog 运行日志环形缓冲（stdout/stderr 镜像），面板「日志」页读它。
	runLog *runlog.Buffer
	// warmMu / warmStat 最近一轮保活预热的参数（target/batch/actual）。
	warmMu   sync.Mutex
	warmStat WarmStat
}

// New 构造前门。
func New(cfg config.Config, p *pool.Pool, up *upstream.Client, log *reqlog.Log, version string) *Server {
	s := &Server{
		cfg:     cfg,
		pool:    p,
		up:      up,
		models:  NewModelCache(up, p),
		log:     log,
		started: time.Now(),
		version: version,
		videos:  newVideoStore(),
		files:   NewFileStore(),
		blobs:   NewBlobStore(),
	}
	// 配置了公网基址就固定下来（服务端到服务端取图时 Host 不是公网域名）。
	if cfg.BlobBaseURL != "" {
		s.blobs.SetBase(cfg.BlobBaseURL)
	}
	return s
}

// SetPanel 挂载管理面板（nil = 不挂）。
func (s *Server) SetPanel(h http.Handler) { s.panel = h }

// Pool 暴露给面板。
func (s *Server) Pool() *pool.Pool { return s.pool }

// Log 暴露给面板。
func (s *Server) Log() *reqlog.Log { return s.log }

// SetRunLog 挂运行日志缓冲（stdout/stderr 镜像）。nil = 面板日志页只显示请求流水。
func (s *Server) SetRunLog(b *runlog.Buffer) { s.runLog = b }

// RunLog 暴露给面板。
func (s *Server) RunLog() *runlog.Buffer { return s.runLog }

// Upstream 暴露给面板。
func (s *Server) Upstream() *upstream.Client { return s.up }

// Config 配置副本（面板只读展示）。
func (s *Server) Config() config.Config { return s.cfg }

// Version 版本号（面板展示）。
func (s *Server) Version() string { return s.version }

// UptimeSeconds 运行时长（秒）。
func (s *Server) UptimeSeconds() int { return int(time.Since(s.started).Seconds()) }

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.wrap("/v1/chat/completions", s.handleChat))
	mux.HandleFunc("POST /v1/messages", s.wrap("/v1/messages", s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.wrap("/v1/messages/count_tokens", s.handleCountTokens))
	mux.HandleFunc("POST /v1/responses", s.wrap("/v1/responses", s.handleResponses))
	mux.HandleFunc("GET /v1/models", s.wrap("/v1/models", s.handleModels))
	mux.HandleFunc("POST /v1/images/generations", s.wrap("/v1/images/generations", s.handleImages))
	mux.HandleFunc("POST /v1/images/edits", s.wrap("/v1/images/edits", s.handleImageEdits))
	mux.HandleFunc("POST /v1/videos/generations", s.wrap("/v1/videos/generations", s.handleVideoCreate))
	mux.HandleFunc("GET /v1/videos/{id}", s.wrap("/v1/videos/{id}", s.handleVideoGet))
	mux.HandleFunc("GET /v1/videos/{id}/content", s.wrap("/v1/videos/{id}/content", s.handleVideoContent))
	mux.HandleFunc("POST /v1/files", s.wrap("/v1/files", s.handleFiles))
	mux.HandleFunc("GET /v1/files/{id}", s.wrap("/v1/files/{id}", s.handleFileGet))
	// 🔴 免鉴权：上游来拉图时不会带我们的 API Key。
	// 安全性靠"不可猜 ID + 2h TTL + 只读"，见 blobs.go 文件头。
	mux.HandleFunc("GET /v1/blobs/{id}", s.handleBlob)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /stats", s.wrap("/stats", s.handleStats))
	if s.panel != nil {
		mux.Handle("/panel/", s.panel)
		// {$} = 精确匹配根路径：否则 "GET /" 会与 "/panel/" 的 method 集合冲突（ServeMux 直接 panic）。
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/panel/", http.StatusFound)
		})
	}
	return s.withCORS(mux)
}

// ---------- 中间件 ----------

// wrap 鉴权 + 日志。
func (s *Server) wrap(path string, h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "无效的 API Key")
			return
		}
		h(w, r)
	}
}

// authorized 校验 API Key。生效列表为空 = 不鉴权（本机/内网场景）。
//
// 列表来源是运行时快照（见 apikeys.go）：面板里撤销/生成 key 立即生效，
// 不需要重启 —— 泄漏一把 key 时这一点比"少一次读锁"重要得多。
func (s *Server) authorized(r *http.Request) bool {
	keys := s.keyStrings()
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
		token = strings.TrimSpace(r.Header.Get("x-api-key")) // Anthropic 客户端习惯
	}
	if token == "" {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// withCORS 允许浏览器端直连（面板自身同源，这里是给第三方前端的）。
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal_error", "网关内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- 运维端点 ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"service":  "qwen2api",
		"version":  s.version,
		"uptime_s": int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready := s.pool.ReadyCount()
	code := http.StatusOK
	status := "ready"
	if ready == 0 {
		code = http.StatusServiceUnavailable
		status = "no_account_ready"
	}
	writeJSON(w, code, map[string]any{
		"status":         status,
		"accounts":       s.pool.Len(),
		"accounts_ready": ready,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// 顺便刷一遍余额（2 分钟 TTL，比视频路径的 60s 松）：
	// 面板要能看到真实余额，但轮询不该把第三方域名打热。
	// 这是 **零花钱** 的读法 —— 不提交任何生成任务。
	s.refreshStaleCredits(r.Context(), creditTTLStats)

	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "qwen2api",
		"version":  s.version,
		"uptime_s": int(time.Since(s.started).Seconds()),
		"pool":     s.pool.Snapshot(),
		"stats":    s.log.Stats(),
		"models":   s.models.Info(),
		// keepalive：最近的预热参数 + 当前新鲜凭据数，用来判断"预热跟不跟得上并发"。
		"keepalive": map[string]any{
			"warm":        s.WarmSnapshot(),
			"token_fresh": s.pool.TokenFreshCount(),
		},
	})
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
}

// writeErrorParam 带 param 的错误（Images 端点用：指明是哪个字段的问题）。
func writeErrorParam(w http.ResponseWriter, code int, typ, msg, param string) {
	writeJSON(w, code, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Param: param}})
}

// clientIP 取调用来源 IP（尊重常见反代头）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sortedKeys 便于稳定输出。
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = fmt.Sprintf
