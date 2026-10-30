package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- OpenAI Videos 前门 ----------
//
// 上游是**异步任务**：POST completions（stream:false）同步回一个 `tsk_…` 任务 id，
// 产物要用 `GET /api/v2/task/status/<id>` 轮询。这与文本/图片的 SSE 增量是两条读法。
//
// 端点按 grok2api 的形态：
//
//	POST /v1/videos/generations  → {id, object:"video", status:"queued", ...}
//	GET  /v1/videos/{id}         → {id, status, url, ...}
//
// 🔴 任务 → 账号的映射只在**内存**里存 2 小时（查询必须用提交它的那个账号的凭据）。
// 进程重启后旧 id 查不到 —— 这是已知边界，不做持久化（任务本身在上游，重查要新 id）。

// VideoRequest `POST /v1/videos/generations`。
type VideoRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	Size   string `json:"size"`
	// Resolution 720P / 1080P（1080P 走付费积分）。
	Resolution string `json:"resolution"`
	// Duration 秒：5 / 10 / 15（各档独立计价）。
	Duration int  `json:"duration"`
	Audio    bool `json:"audio"`
	// Seconds / Width / Height 兼容 OpenAI/Sora 风格客户端，内部折算成 size。
	Seconds int `json:"seconds"`
	Width   int `json:"width"`
	Height  int `json:"height"`
}

// VideoObject 对外视频对象。
type VideoObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	Model     string `json:"model"`
	Status    string `json:"status"` // queued / in_progress / completed / failed
	Progress  int    `json:"progress"`
	URL       string `json:"url,omitempty"`
	// GatewayURL 网关代取地址（上游 CDN 有防盗链，裸链直下会 404）。
	GatewayURL string `json:"gateway_url,omitempty"`
	Error      string `json:"error,omitempty"`
	// UpstreamID 上游任务 id（tsk_…），便于对照 qwen 控制台排查。
	UpstreamID string `json:"upstream_id,omitempty"`
}

type videoTask struct {
	account  string
	taskID   string
	model    string
	created  time.Time
	lastSeen string // 最近一次上游状态原文（排查用）
}

// videoStore 任务 → 账号映射（内存，2h TTL）。
type videoStore struct {
	mu    sync.Mutex
	items map[string]*videoTask
}

func newVideoStore() *videoStore { return &videoStore{items: map[string]*videoTask{}} }

func (v *videoStore) put(id string, t *videoTask) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.items[id] = t
	// 顺手清过期（量小，线性扫足够）。
	for k, item := range v.items {
		if time.Since(item.created) > 2*time.Hour {
			delete(v.items, k)
		}
	}
}

func (v *videoStore) get(id string) (*videoTask, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	t, ok := v.items[id]
	if !ok || time.Since(t.created) > 2*time.Hour {
		return nil, false
	}
	return t, true
}

// handleVideoCreate `POST /v1/videos/generations`。
func (s *Server) handleVideoCreate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID: "vid-" + upstream.UUID()[:8], Path: "/v1/videos/generations",
		ClientIP: clientIP(r), UserAgent: truncateUA(r.UserAgent()),
	}

	var req VideoRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "解析请求体: " + err.Error()
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "prompt 不能为空", "prompt")
		return
	}

	// 画幅：优先 size；否则由 width/height 折算；再否则用配置默认。
	size := strings.TrimSpace(req.Size)
	if size == "" && req.Width > 0 && req.Height > 0 {
		size = sizeToRatio(strings.ToLower(
			// 复用像素→比例的映射
			itoa(req.Width) + "x" + itoa(req.Height)))
	}
	if size == "" {
		size = s.cfg.Video.Size
	}
	resolution := strings.TrimSpace(req.Resolution)
	if resolution == "" {
		resolution = s.cfg.Video.Resolution
	}
	duration := req.Duration
	if duration <= 0 {
		duration = req.Seconds
	}
	if duration <= 0 {
		duration = s.cfg.Video.Duration
	}
	audio := req.Audio || s.cfg.Video.Audio

	// model 字段可能填 chat 模型，也可能填视频模型（清单里两者都在）。
	// chatModel 用于建会话，rootModel 是生成内核；对外的 model 回显请求原值。
	requested := strings.TrimSpace(req.Model)
	chatModel, rootModel := s.resolveVideoModel(requested)
	model := requested
	if model == "" {
		model = rootModel
	}

	// 积分：按档位算这一次要扣多少。查不到档位就明确拒绝 ——
	// 上游只支持 5/10/15 秒，瞎填的时长本该在网关拦掉，不该放过去烧积分。
	needCredits, ok := s.cfg.Video.VideoCredits(resolution, duration)
	if !ok {
		if len(s.cfg.Video.Pricing) == 0 {
			needCredits = 0 // 未启用积分模式：退回个数口径
		} else {
			entry.Status = http.StatusBadRequest
			entry.Error = "不支持的视频档位"
			s.addReq(&entry)
			writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("不支持的档位 %s/%ds；支持的档位见 config.video.pricing（默认 720P|1080P × 5|10|15s）",
					resolution, duration), "resolution")
			return
		}
	}

	spec := upstream.VideoSpec(size, resolution, duration, audio, rootModel)

	var (
		acct    string
		taskID  string
		lastErr error
	)
	for attempt := 0; attempt < s.cfg.Pool.MaxRotate; attempt++ {
		// 视频按积分计费（720P 5s=30、1080P 15s=120），选号必须看"剩余预算够不够这一次"。
		a, ok := s.pool.PickForVideo(needCredits)
		if !ok {
			if lastErr != nil {
				break
			}
			// 先分辨"钱不够"与"账号都不可用"：两者都让 Pick 失败，
			// 但调用方的处置完全不同（一个该充值/等额度、一个该等冷却或加号）。
			if sent, msg, short := s.creditsShortfall(needCredits); short {
				lastErr = fmt.Errorf("%w：%s", sent, msg)
				break
			}
			lastErr = errNoAccount
			break
		}
		s.egressOf(a.Name) // 视频要跑十几分钟，探测早回来了
		// 余额是这个号"能不能做这一步"的唯一依据。选号之后只刷**它**：
		// 池子 3000+ 号时全池刷新既慢又没意义（绝大多数号根本不参与这次请求）。
		// 刷不到就退回不拦，由上游 insufficient_quota 兜底。
		if _, known := s.pool.CreditBalance(a.Name); !known {
			s.refreshOneCredit(r.Context(), a.Name)
		}
		if bal, known := s.pool.CreditBalance(a.Name); known && needCredits > 0 && bal < needCredits {
			// 🔴 先 Release 再 continue：Pick 已经把 inflight +1，
			// 漏掉这一步这个号会被永久算作"在途"，再也选不中（静默掉号）。
			s.pool.Release(a)
			// 余额已写进池子，下一轮 PickForVideo 会自动跳过它。
			continue
		}

		attemptStart := time.Now()
		cred := s.pool.Credential(a.Name)
		actx := upstream.WithAccount(r.Context(), a.Name)
		chatID, err := s.takeChat(actx, a.Name, cred, chatModel, spec.ChatType)
		if err == nil {
			body := upstream.BuildSubmitSpec(chatID, "", chatModel, req.Prompt, "fast", spec, time.Now())
			taskID, err = s.up.SubmitVideo(actx, cred, body)
			if err == nil {
				acct = a.Name
				s.pool.MarkOK(a)
				s.pool.Release(a)
				break
			}
		}
		s.pool.Release(a)
		s.noteFailure(a, err)
		lastErr = err
		// 与聊天/生图同口径：语义未受理 或 **快速失败**（提交本身就几百毫秒，
		// 几秒就挂说明没建任务）都换号；慢失败不重试（提交可能已扣积分）。
		if !upstream.RetrySafe(err) && !fastRetryable(err, time.Since(attemptStart)) {
			break
		}
	}
	if taskID == "" {
		// 全池钱不够时给明确信号，而不是笼统的"没有可用账号"。
		if lastErr == errNoAccount || lastErr == nil {
			if sent, msg, short := s.creditsShortfall(needCredits); short {
				lastErr = fmt.Errorf("%w：%s", sent, msg)
			}
		}
		if lastErr == nil {
			lastErr = errNoAccount
		}
		code := httpStatusFor(lastErr)
		entry.Status = code
		entry.Error = lastErr.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(&entry)
		writeError(w, code, errTypeFor(lastErr), lastErr.Error())
		return
	}

	// 🔴 计数点在"拿到 task_id"：视频是异步任务，提交即扣上游积分，
	// 产物要 15 分钟后才知道 —— 等下载成功再计数就漏计了。
	s.pool.MarkVideoDone(s.pool.Get(acct))
	if needCredits > 0 {
		s.pool.SpendVideoCredits(s.pool.Get(acct), needCredits)
		// 余额缓存 60s 才刷一次；乐观扣减让同一分钟内的下一个请求看到正确数字。
		s.pool.DebitCreditBalance(acct, needCredits)
	}

	// 对外 id 用 video_ 前缀，避免客户端把上游 tsk_ 当成我们的资源 id。
	id := "video_" + strings.TrimPrefix(taskID, "tsk_")
	s.videos.put(id, &videoTask{account: acct, taskID: taskID, model: model, created: time.Now()})

	writeJSON(w, http.StatusOK, VideoObject{
		ID: id, Object: "video", CreatedAt: time.Now().Unix(), Model: model,
		Status: "queued", UpstreamID: taskID,
	})

	entry.Model = model
	entry.Account = acct
	entry.Status = http.StatusOK
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(&entry)
}

// handleVideoGet `GET /v1/videos/{id}`。
func (s *Server) handleVideoGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少视频 id")
		return
	}
	task, ok := s.videos.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			"未知的视频 id（任务映射只在内存保留 2 小时，进程重启后旧 id 失效）")
		return
	}
	cred := s.pool.Credential(task.account)
	if cred == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "任务所属账号已不在池里")
		return
	}

	raw, err := s.up.TaskStatus(upstream.WithAccount(r.Context(), task.account), cred, task.taskID)
	if err != nil {
		writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
		return
	}
	obj := translateVideo(task, raw)
	// 客户端拿到 CDN 直链直接下载会 404（防盗链要 Referer），所以再给一个网关代理地址。
	if obj.URL != "" {
		obj.GatewayURL = "/v1/videos/" + id + "/content"
	}
	writeJSON(w, http.StatusOK, obj)
}

// handleVideoContent `GET /v1/videos/{id}/content` —— 代理下载成片。
//
// 为什么需要它：上游 CDN 有防盗链，客户端拿裸链接直接下会得到 404（226 字节 HTML）。
// 网关加 Referer 代取，客户端就能像普通文件一样下载。
func (s *Server) handleVideoContent(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/videos/"), "/content")
	task, ok := s.videos.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "未知的视频 id")
		return
	}
	cred := s.pool.Credential(task.account)
	if cred == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "任务所属账号已不在池里")
		return
	}
	raw, err := s.up.TaskStatus(upstream.WithAccount(r.Context(), task.account), cred, task.taskID)
	if err != nil {
		writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
		return
	}
	obj := translateVideo(task, raw)
	if obj.URL == "" {
		writeError(w, http.StatusConflict, "invalid_request_error",
			"任务还没有产物（status="+obj.Status+"）")
		return
	}
	data, err := fetchMedia(r.Context(), obj.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "取回产物失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", itoa(len(data)))
	w.Header().Set("Content-Disposition", `inline; filename="`+id+`.mp4"`)
	_, _ = w.Write(data)
}

// translateVideo 上游任务对象 → OpenAI videos 形态。
//
// 上游的 task_status 取值域没有官方文档；这里按**包含关系**映射（不猜精确枚举），
// 并把原文放进 upstream_status 供排查 —— 状态判错时至少能看见原始值。
func translateVideo(task *videoTask, raw map[string]any) VideoObject {
	status := strings.ToLower(strings.TrimSpace(str(raw["task_status"])))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(str(raw["status"])))
	}
	out := VideoObject{
		ID: "video_" + strings.TrimPrefix(task.taskID, "tsk_"), Object: "video",
		CreatedAt: task.created.Unix(), Model: task.model, UpstreamID: task.taskID,
	}
	switch {
	case strings.Contains(status, "success"), strings.Contains(status, "completed"),
		strings.Contains(status, "done"), strings.Contains(status, "finish"):
		out.Status = "completed"
		out.Progress = 100
		out.URL = extractVideoURL(raw)
	case strings.Contains(status, "fail"), strings.Contains(status, "error"):
		out.Status = "failed"
		out.Error = "上游任务失败：" + status
	default:
		out.Status = "in_progress"
		// 上游不报百分比；有就透传，没有就留 0（不编进度条）。
		if p, ok := raw["progress"].(float64); ok {
			out.Progress = int(p)
		}
	}
	task.lastSeen = status
	return out
}

// extractVideoURL 从任务对象里找产物地址（上游字段名不固定，按已知候选顺序取）。
func extractVideoURL(raw map[string]any) string {
	// data.content 在 rsfree 的实测里是产物 URL。
	if s, ok := raw["content"].(string); ok && strings.HasPrefix(s, "http") {
		return s
	}
	for _, key := range []string{"video_url", "url", "output_url"} {
		if s, ok := raw[key].(string); ok && strings.HasPrefix(s, "http") {
			return s
		}
	}
	// contents / videos 数组里第一个带 url 的条目。
	for _, key := range []string{"contents", "videos", "outputs"} {
		if arr, ok := raw[key].([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					for _, k := range []string{"video_url", "url"} {
						if s, ok := m[k].(string); ok && strings.HasPrefix(s, "http") {
							return s
						}
					}
				}
			}
		}
	}
	return ""
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
