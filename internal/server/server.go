// Package server 是 OpenAI 兼容前门：/v1/chat/completions、/v1/messages、
// /v1/responses、/v1/models，外加运维面 /healthz /readyz /stats 与 /panel/。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/runlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// Server HTTP 前门。
type Server struct {
	// cfg 运行时配置快照（面板可热更）。用 atomic.Value 存**整份** config，
	// 读取走 s.conf()：改一个字段也是整体替换，读侧永远看到一致的快照
	// （不会出现"max_rotate 已改、cooldown 还是旧值"的半更新状态）。
	cfg     atomic.Value // config.Config
	pool    *pool.Pool
	up      *upstream.Client
	models  *ModelCache
	log     *reqlog.Log
	started time.Time
	version string
	panel   http.Handler
	// videos 视频任务 → 账号映射。落盘在 data/videos.json（见 videostore.go）：
	// 上游任务活 7 天、产物 24h，只存内存等于重启即丢客户端的任务 id。
	videos *videoStore
	// videoStop 关闭后台巡检（回调推送依赖它）。
	videoStop     chan struct{}
	videoStopOnce sync.Once
	// polish 视频后处理队列（去水印 + 上传自建存储）。
	// nil 表示未启用 storage —— 此时退回"直接透传上游直链"的旧行为。
	polish *videoPolisher
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
	// refreshErrAt 预热续期失败的时间戳（账号名 → 上次失败）：
	// 同一份数据两个用途 —— 日志去重 + 重试退避（见 refreshDue）。
	refreshErrAt map[string]time.Time
	// refreshLocks 按账号分片的"换票互斥"（见 lockRefresh）。
	// 预热 refresh、请求路径 relogin、后台 signin 三者必须串行：同时换同一个号
	// 会互相作废对方刚拿到的票（实测 401「令牌已撤销」的嫌疑来源之一），
	// 而每次多余失败都要用密码 signin 收尾。
	refreshLocks [refreshLockShards]sync.Mutex
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
	// inflightNow 当前在途的 chat 请求数；inflightPeak 是"自上次读取以来"的峰值。
	//
	// 为什么要峰值而不是瞬时值：预热每 20 秒才看一次，瞬时采样会漏掉两次 tick
	// 之间那一下突发（而突发正是要覆盖的场景）。请求进出时顺手 CAS 一下峰值，
	// 预热读完 Swap(0) 清零，下一轮重新统计。
	//
	// 用途：会话与连接的覆盖范围按它算（不是按 token 的 target）——
	// 同时在跑的 N 个请求才需要 N 个不同号，多保活全是白打代理。
	inflightNow  atomic.Int32
	inflightPeak atomic.Int32
	// chatTargetNow 上一轮会话/连接预热覆盖了多少个号（按并发算出来的）。
	// 与 warm.target 并列展示，一眼能看出"两个口径差多少"。
	chatTargetNow atomic.Int32
	// chatCursor 预热轮转游标（见 prewarmChats）。
	//
	// 🔴 必须有它：HotNames 的排序在"预热不改变 served"时是稳定的，
	// 每轮只取前 limit 个就等于永远只热同一小批 —— 后面那 100 多个号
	// 每次被选中都是冷连接（实测 submit 3~6 秒 vs 945ms）。
	chatCursor atomic.Uint32
	// egressMu / egress 账号级"出口 IP"缓存（懒探测，见 egress.go）。
	egressMu sync.Mutex
	egress   map[string]*egressState
	// chats 预建的空会话池（按账号 + 会话类型分组，见 chatpool.go）。
	chats *ChatPool
	// dialogs Agent 模型的对话粘性表（见 dialog.go）。纯内存，重启即丢。
	dialogs *dialogStore
	// cfgPath 配置文件路径（面板保存配置时要写回它；空 = 不可写，只读展示）。
	cfgPath string
}

// storageExplicitlyOff 配置里是否显式写了 enabled=false（用于日志分流：
// "主动关"不该报"缺配置"）。
func storageExplicitlyOff(st config.Storage) bool {
	return st.Enabled != nil && !*st.Enabled
}

// New 构造前门。
func New(cfg config.Config, p *pool.Pool, up *upstream.Client, log *reqlog.Log, version string) *Server {
	s := &Server{
		pool:      p,
		up:        up,
		models:    NewModelCache(up, p),
		log:       log,
		started:   time.Now(),
		version:   version,
		videos:    newVideoStore(config.VideosFile(cfg.StateFile)),
		videoStop: make(chan struct{}),
		files:     NewFileStore(),
		blobs:     NewBlobStore(),
		egress:    map[string]*egressState{},
		chats:     NewChatPool(cfg.Pool.WarmChatsPerAccount, time.Duration(cfg.Pool.ChatPoolTTLS)*time.Second),
		dialogs:   newDialogStore(),
	}
	s.cfg.Store(cfg)
	// 后处理队列：托管开启时起 worker。
	if cfg.Storage.IsEnabled() {
		s.polish = newVideoPolisher(s, cfg.Storage.Workers)
		stdlog.Printf("[qwen2api] 视频托管已启用：%s/%s 去水印=%v 画质=%s 并发=%d",
			cfg.Storage.Endpoint, cfg.Storage.Bucket,
			cfg.Storage.StripWatermark, cfg.Storage.Quality, cfg.Storage.Workers)
	} else if miss := cfg.Storage.MissingKeys(); len(miss) > 0 && !storageExplicitlyOff(cfg.Storage) {
		// 默认开启但凭据不全：降级而不是拒绝启动。
		// 🔴 必须吵一句：否则运维以为在去水印，实际客户端拿到的是带水印直链。
		stdlog.Printf("[qwen2api] ⚠️ 视频托管默认开启，但缺少配置 %v —— "+
			"已降级为透传上游直链（成片**带水印**且 24h 过期）", miss)
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
// Config 当前配置快照（值拷贝，供面板/统计读）。
func (s *Server) Config() config.Config { return s.conf() }

// SetConfigPath 告知配置文件在哪（面板保存时写回它）。
func (s *Server) SetConfigPath(p string) { s.cfgPath = p }

// ConfigPath 配置文件路径（空 = 面板只能看不能改）。
func (s *Server) ConfigPath() string { return s.cfgPath }

// UpdateConfig 把面板表单的改动合并进配置文件并**热更新**。
//
// 流程：读原文件 → 按白名单校验合并（config.Apply）→ 原子写回 → 用新文件重建
// 配置快照并应用。任何一步失败都不留半改状态：写文件用 tmp + rename。
//
// 🔴 只改面板列出的字段（白名单），且 accounts / api_keys / panel 这类
// "写错就锁死服务"的项根本不在白名单里 —— 它们只能改文件 + 重启。
func (s *Server) UpdateConfig(patch map[string]any) (applied, needRestart []string, err error) {
	path := s.cfgPath
	if path == "" {
		return nil, nil, errors.New("未指定配置文件（启动时没给 -config），只能查看不能修改")
	}
	raw := map[string]any{}
	b, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if e := json.Unmarshal(b, &raw); e != nil {
			return nil, nil, fmt.Errorf("现有配置文件不是合法 JSON：%w", e)
		}
	case os.IsNotExist(rerr):
		// 文件不存在（纯环境变量部署）：从当前快照起步，写出一份完整配置。
		snap, _ := json.Marshal(s.conf())
		_ = json.Unmarshal(snap, &raw)
	default:
		return nil, nil, rerr
	}

	applied, needRestart, err = config.Apply(raw, patch)
	if err != nil {
		return nil, nil, err
	}
	if len(applied) == 0 {
		return nil, nil, nil // 没有实际变化
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	out = append(out, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, nil, fmt.Errorf("写临时文件失败：%w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// 🔴 容器里 config.json 常是**单文件 bind mount**（-v .../config.json:/app/config.json），
		// 这种挂载点是内核层面的"设备节点"，rename 上去会报
		// `device or resource busy`（2026-10-31 线上实测）。
		// 降级：先备份，再原地覆盖写（文件本身可写，只是不能换 inode）。
		_ = os.Remove(tmp)
		bak := path + ".bak"
		if b, e := os.ReadFile(path); e == nil {
			_ = os.WriteFile(bak, b, 0o600)
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if e != nil {
			return nil, nil, fmt.Errorf("写配置文件失败（rename 与覆盖写都失败：%v / %w）", err, e)
		}
		if _, e = f.Write(out); e != nil {
			f.Close()
			return nil, nil, fmt.Errorf("写配置文件失败：%w", e)
		}
		if e = f.Close(); e != nil {
			return nil, nil, fmt.Errorf("写配置文件失败：%w", e)
		}
		stdlog.Printf("[qwen2api] 配置文件是单文件挂载，已改用覆盖写（备份在 %s）", bak)
	}

	// 用新文件重建快照：从当前配置拷贝一份再 unmarshal —— 文件里没有的字段
	// （accounts / 环境变量注入的项）保持原值，不会被抹成零值。
	nc := s.conf()
	if err := json.Unmarshal(out, &nc); err != nil {
		return applied, needRestart, fmt.Errorf("新配置解析失败（已写入文件，重启后生效）：%w", err)
	}
	s.ApplyConfig(nc)
	for _, k := range applied {
		stdlog.Printf("[qwen2api] 配置热更 %s", k)
	}
	return applied, needRestart, nil
}

// conf 当前运行时配置（指针，只读使用；不要修改返回值）。
func (s *Server) conf() config.Config {
	v, _ := s.cfg.Load().(config.Config)
	return v
}

// ApplyConfig 热更新配置：整体替换快照 + 同步池与会话池的参数。
//
// 只用于**支持热更**的字段（见 panel 的 configFields 白名单）：
// listen / accounts / api_keys / panel / upstream.base_url 改了必须重启，
// 面板会明确标出来，不会假装生效。
func (s *Server) ApplyConfig(nc config.Config) {
	// accounts 属于"启动期资源"：面板没有账号编辑能力，热更时保留原值，
	// 避免一次配置保存把整池账号从内存里抹掉。
	nc.Accounts = s.conf().Accounts
	s.cfg.Store(nc)
	// 池参数（限额/冷却/轮换）立即生效 —— 它们只在选号与冷却计算时读。
	s.pool.SetConfig(nc.Pool)
	// 会话池的 per/ttl 也随配置变（0 就是关掉预建）。
	s.chats.SetLimits(nc.Pool.WarmChatsPerAccount,
		time.Duration(nc.Pool.ChatPoolTTLS)*time.Second)
}

// Version 版本号（面板展示）。
func (s *Server) Version() string { return s.version }

// UptimeSeconds 运行时长（秒）。
func (s *Server) UptimeSeconds() int { return int(time.Since(s.started).Seconds()) }

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	// 视频后台巡检：刷新未终态任务 + 推回调。
	// 没有它，带 callback_url 的任务只能等客户端自己来查才动 —— 那就不是回调了。
	//
	// 只在**有落盘路径**时启动：空路径意味着这是个无状态实例（测试 / 未配 data 卷），
	// 后台任务既存不住也没有回调对象，起了只是白白打上游。
	if s.videos.Path() != "" {
		s.videoStopOnce.Do(func() { go s.videoWatchLoop(s.videoStop) })
	}
	// 重启恢复：把停在 queued/running 的后处理任务重新排上。
	// 没有这一步，重启那一刻在处理的视频会永远卡在 polishing。
	if s.polish != nil {
		go s.polish.recover()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.wrap("/v1/chat/completions", s.handleChat))
	mux.HandleFunc("POST /v1/messages", s.wrap("/v1/messages", s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.wrap("/v1/messages/count_tokens", s.handleCountTokens))
	mux.HandleFunc("POST /v1/responses", s.wrap("/v1/responses", s.handleResponses))
	mux.HandleFunc("GET /v1/models", s.wrap("/v1/models", s.handleModels))
	mux.HandleFunc("POST /v1/images/generations", s.wrap("/v1/images/generations", s.handleImages))
	mux.HandleFunc("POST /v1/images/edits", s.wrap("/v1/images/edits", s.handleImageEdits))
	// 视频：火山方舟（Ark）原生协议。
	// 路径逐字等于方舟数据面，客户端 base_url 设 https://<host>/api/v3 即可直接对接。
	mux.HandleFunc("POST "+videoTaskPath, s.wrap(videoTaskPath, s.handleVideoCreate))
	mux.HandleFunc("GET "+videoTaskPath, s.wrap(videoTaskPath, s.handleVideoList))
	mux.HandleFunc("GET "+videoTaskPath+"/{id}", s.wrap(videoTaskPath+"/{id}", s.handleVideoGet))
	mux.HandleFunc("DELETE "+videoTaskPath+"/{id}", s.wrap(videoTaskPath+"/{id}", s.handleVideoDelete))
	mux.HandleFunc("GET "+videoTaskPath+"/{id}/content", s.wrap(videoTaskPath+"/{id}/content", s.handleVideoContent))
	// 旧出口全部 410：两套形状并存必然出现「两边字段对不上」。
	mux.HandleFunc("/v1/videos", s.wrap("/v1/videos", s.handleVideoLegacy))
	mux.HandleFunc("/v1/videos/", s.wrap("/v1/videos/", s.handleVideoLegacy))
	mux.HandleFunc("POST /v1/files", s.wrap("/v1/files", s.handleFiles))
	mux.HandleFunc("GET /v1/files/{id}", s.wrap("/v1/files/{id}", s.handleFileGet))
	// 🔴 免鉴权：上游来拉图时不会带我们的 API Key。
	// 安全性靠"不可猜 ID + 2h TTL + 只读"，见 blobs.go 文件头。
	mux.HandleFunc("GET /v1/blobs/{id}", s.handleBlob)
	// 🔴 同样免鉴权：浏览器 <img> 不会带 API Key。
	// 只代理写死的上游 host + /output/ 前缀，安全性见 mediaproxy.go 文件头。
	mux.HandleFunc("GET /v1/media/{rest...}", s.handleImageProxy)
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
			// 带 code 而不是裸两键：鉴权是**横切**错误，视频端点的方舟客户端
			// 会按 error.code 分支（换 key / 放弃重试），这里缺了就拿到空值。
			// 同一份信封服务所有端点，所以码只能取一套词表 —— 用方舟的
			// AuthenticationError；OpenAI 客户端不读 code 的取值，不受影响。
			writeErrorCode(w, http.StatusUnauthorized, "authentication_error",
				errCodeAuth, "", "无效的 API Key")
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
				stdlog.Printf("[panic] %s %s: %v", r.Method, r.URL.Path, rec)
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
		// keepalive：预热参数 + 当前新鲜凭据数，用来判断"预热跟不跟得上并发"。
		"keepalive": map[string]any{
			// params 是**当前生效值**（面板热更后不用猜有没有生效）。
			"params":      s.conf().Pool.Keepalive.Effective(),
			"warm":        s.WarmSnapshot(),
			"token_fresh": s.pool.TokenFreshCount(),
			// chats 预建会话池：ready = 当前可直接用的空会话数。
			"chats": s.chats.Stat(),
			// inflight 当前在途；chat_target 会话/连接这轮覆盖了几个号
			// （按并发算，通常远小于 warm.target —— 这两个数差太多是正常的）。
			"inflight":    s.inflightNow.Load(),
			"chat_target": s.chatTargetNow.Load(),
			// dialogs Agent 对话粘性表：entries = 活跃对话数。
			// 长期贴着 max 说明 TTL 偏长或客户端在刷 key；长期 0 说明没人在用 agent。
			"dialogs": s.dialogs.snapshot(),
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
	typ, msg = clientError(code, typ, msg)
	retryAfter(w, code)
	writeJSON(w, code, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
}

// retryAfter 给"过会儿再来"的 5xx 补上标准重试提示。
//
// 为什么值得单独写：客户端与中间网关（new-api / one-api 这类）很多只在
// 看到 Retry-After 时才自动重试；缺了它，一次上游抖动就变成调用方的一次硬失败。
// 5 秒是保守值 —— 上游"零产物"通常是单个账号挂了，换号重发立刻就能出。
func retryAfter(w http.ResponseWriter, code int) {
	switch code {
	case http.StatusGatewayTimeout, http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "5")
	}
}

// itoa 小整数转字符串（内联友好，热路径上不必走 fmt）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// errCodeAuth 鉴权失败的 code（方舟词表）。
//
// 🔴 为什么放在信封这一层而不是 videoprotocol.go：401 由 `wrap` 产生，
// 作用于 chat / images / messages / videos 全部端点。协议专属的码（MissingParameter
// 之类）跟着各自协议层走，横切码跟着信封走 —— 否则 chat 请求会因为「视频协议的常量」
// 而依赖一个它根本不该知道的文件。
const errCodeAuth = "AuthenticationError"

// writeErrorCode 带 code 的四键信封。
//
// canonical 视频协议要求 `{error:{message,type,param,code}}`：
// code 与 type 分工不同 —— type 说「哪一类错」，code 说「具体是什么」。
// 只给 type 时客户端只能按大类分支（400 一律当参数错），归因粒度不够。
func writeErrorCode(w http.ResponseWriter, code int, typ, errCode, param, msg string) {
	typ, msg = clientError(code, typ, msg)
	retryAfter(w, code)
	writeJSON(w, code, ErrorBody{Error: ErrorDetail{
		Message: msg, Type: typ, Param: param, Code: errCode,
	}})
}

// writeErrorParam 带 param 的错误（Images 端点用：指明是哪个字段的问题）。
func writeErrorParam(w http.ResponseWriter, code int, typ, msg, param string) {
	typ, msg = clientError(code, typ, msg)
	retryAfter(w, code)
	writeJSON(w, code, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Param: param}})
}

// clientError 把"服务端 / 上游的问题"统一成标准英文文案。
//
// 规则（2026-10-30 定）：
//   - **5xx**（上游超时 / 故障 / 凭据坏了）⇒ 短英文 + 稳定 type：
//     客户端要的是机器可读的失败信号，不是一段中文排查指南（那种文案换个人写就变）。
//   - 4xx 与 429 ⇒ 原样返回：429 说明"是哪个额度、还差多少"，调用方和运维都用得上；
//     4xx 是调用方自己写错了，具体到字段的中文提示最省事。
//
// 5xx 的详细原因（"额度将尽 / 上游临时故障 / 换号或稍等"这类判断）只写进请求流水与
// 容器日志（reqlog.Entry.Error），面板「日志」页照常看得到 —— 不丢信息，只是不外泄。
func clientError(code int, typ, msg string) (string, string) {
	if code < 500 {
		// 429 分两种：额度不足（"还差多少积分"对调用方有用，保留中文）
		// 与风控/限流（"上游 x5sec 风控"这种原文对客户端毫无意义 ⇒ 英文短句）。
		if code == http.StatusTooManyRequests && typ == "rate_limit_error" {
			return typ, "upstream rate limited, please retry later"
		}
		return typ, msg
	}
	switch typ {
	case "upstream_timeout":
		return typ, "upstream timeout, please retry later"
	case "service_unavailable":
		return typ, "no available upstream account, please retry later"
	case "insufficient_quota":
		return typ, "upstream quota exceeded"
	case "rate_limit_error":
		return typ, "upstream rate limited, please retry later"
	case "upstream_auth_error":
		// 网关自己的上游凭据坏了：不把"哪个号、什么错"递给调用方。
		return "upstream_error", "upstream authentication failed"
	}
	return "upstream_error", "upstream error, please retry later"
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
