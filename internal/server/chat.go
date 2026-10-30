package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// errNoAccount 池里没有可用账号。
var errNoAccount = errors.New("当前没有可用账号：全部处于冷却/失效状态，或未配置账号")

// errVideoQuotaLocal 本地每日视频配额（配置的每日预算）用尽。
var errVideoQuotaLocal = errors.New("本地每日视频配额已用尽")

// errCreditsShort 账号积分余额不足（上游账本口径，不是我们设的预算）。
//
// 与 errVideoQuotaLocal 分开是因为**处置完全不同**：
//   - 预算用尽 ⇒ 今天就是不能做了，改配置或等明天；
//   - 余额不足 ⇒ 可以充值，或换一个有余额的号，或等每日赠送。
//
// 两个都映射 429/insufficient_quota，但文案不能混 —— 混了就会让人去改一个
// 根本不生效的配置项（2026-10-29 实测踩到：报了"本地配额用尽"其实该充值）。
var errCreditsShort = errors.New("账号积分余额不足")

// errImageQuotaLocal 本地每日出图配额用尽（网关自己的限制，不是上游报错）。
//
// 与 upstream.ErrQuota 的区别：那个是**上游**说没额度了；
// 这个是**我们**主动不去撞上游硬限（打到硬限只会静默挂起 180s，体验更差）。
var errImageQuotaLocal = errors.New("本地每日出图配额已用尽")

// opened 一次成功建立的上游会话。
type opened struct {
	acct   *pool.Account
	stream *upstream.Stream
	chatID string
}

// handleChat `POST /v1/chat/completions`。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID:        "req-" + upstream.UUID()[:8],
		Path:      "/v1/chat/completions",
		ClientIP:  clientIP(r),
		UserAgent: truncateUA(r.UserAgent()),
	}

	var req ChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "解析请求体: " + err.Error()
		s.log.Add(entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		entry.Status = http.StatusBadRequest
		entry.Error = "messages 为空"
		s.log.Add(entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	flat, err := Flatten(req.Messages)
	if err != nil {
		code := http.StatusBadRequest
		typ := "invalid_request_error"
		if errors.Is(err, ErrImageUnsupported) {
			typ = "unsupported_feature"
		}
		entry.Status = code
		entry.Error = err.Error()
		s.log.Add(entry)
		writeError(w, code, typ, err.Error())
		return
	}

	// 附件：content part 里的 file_id → 上游 files[] 条目（账号绑定的暂存）。
	files, filesErr := s.collectFiles(req.Messages)
	if filesErr != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = filesErr.Error()
		s.log.Add(entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", filesErr.Error(), "file_id")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.cfg.DefaultModel
	}
	// 清单里同时列了图片/视频模型（见 mediaModels）。拿它们调 chat 时给一句
	// 能指导处置的话 —— 不拦的话模型名会被甩给上游，换回一个 Bad_Request，
	// 调用方只会以为"网关坏了"。
	if ep := s.mediaEndpoint(model); ep != "" {
		entry.Model = model
		entry.Status = http.StatusBadRequest
		entry.Error = "生成模型不能用于 chat: " + model
		s.log.Add(entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("模型 %s 是生成模型，不能用于 chat；请改调 %s", model, ep), "model")
		return
	}
	gear := req.ResolveThinking(s.cfg.DefaultThinking)
	entry.Model = model
	entry.Stream = req.Stream
	entry.Degraded = flat.Degraded

	// 文件解析仍走 t2t（2026-10-28 实测：t2t + files[] 就能读文件内容）。
	spec := upstream.TextSpec()
	spec.Files = files
	sd, err := s.openUpstreamSpec(r.Context(), model, gear, flat.Prompt, spec)
	if err != nil {
		entry.Status = httpStatusFor(err)
		entry.Error = err.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.log.Add(entry)
		writeError(w, entry.Status, errTypeFor(err), err.Error())
		return
	}
	defer sd.stream.Close()
	defer s.pool.Release(sd.acct)
	entry.Account = sd.acct.Name

	if req.Stream {
		s.serveChatStream(w, r, sd, req, model, &entry, start)
	} else {
		s.serveChatOnce(w, r, sd, req, model, &entry, start)
	}
	s.pool.MarkOK(sd.acct)
}

// openUpstream 选号 → 建会话 → 开流，失败按可重试性换号。
//
// 🔴 换号必须发生在**写响应之前**：一旦开始向客户端写 SSE，就再也回不了
// 正经 HTTP 状态，只能落成流内错误。
func (s *Server) openUpstream(ctx context.Context, model, gear, prompt string) (*opened, error) {
	return s.openUpstreamSpec(ctx, model, gear, prompt, upstream.TextSpec())
}

// openUpstreamSpec 同上，带类型规格（t2i 的图片模型 / t2v 的 biz_data 在 spec 里）。
func (s *Server) openUpstreamSpec(ctx context.Context, model, gear, prompt string, spec upstream.ChatSpec) (*opened, error) {
	var lastErr error
	for attempt := 0; attempt < s.cfg.Pool.MaxRotate; attempt++ {
		acct, ok := s.pool.Pick()
		if !ok {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errNoAccount
		}
		// 凭据从池里取实时值（可能刚被自动续期改过），不要缓存 acct.Token。
		cred := s.pool.Credential(acct.Name)
		// 出口跟着账号走：带上账号标记，代理路由据此选固定 IP。
		actx := upstream.WithAccount(ctx, acct.Name)
		chatID, err := s.up.NewChatTyped(actx, cred, model, spec.ChatType)
		if err == nil {
			body := upstream.BuildSubmitSpec(chatID, "", model, prompt, gear, spec, time.Now())
			var st *upstream.Stream
			st, err = s.up.OpenChat(actx, cred, body)
			if err == nil {
				return &opened{acct: acct, stream: st, chatID: chatID}, nil
			}
		}
		s.pool.Release(acct)
		// 凭据失效且配了密码 ⇒ 自动重登一次，然后**重试同一个号**
		// （不消耗换号预算：这不是"这个号不行"，是"这张票过期了"）。
		if upstream.KindOf(err) == upstream.KindAuth && acct.Password != "" {
			if s.relogin(ctx, acct) {
				continue
			}
		}
		s.noteFailure(acct, err)
		lastErr = err
		if !upstream.RetrySafe(err) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errNoAccount
	}
	return nil, lastErr
}

// relogin 用账号密码重新登录，把新凭据写回池。成功返回 true。
func (s *Server) relogin(ctx context.Context, acct *pool.Account) bool {
	if acct.Email == "" || acct.Password == "" {
		return false
	}
	// 🔴 必须带账号上下文：登录要走**这个号的固定出口**。
	// 不带的话 SignIn 会从服务器真实 IP 直连上游 —— 与它平时的出口不一致，
	// 既容易吃 WAF（实测：3128 个号批量登录后，直连登录全被挑战页拦下），
	// 也正是风控画像里"账号登录地突变"的信号。
	cred, err := s.up.SignIn(upstream.WithAccount(ctx, acct.Name), acct.Email, acct.Password)
	if err != nil {
		log.Printf("[qwen2api] 账号 %s 自动重登失败: %v", acct.Name, err)
		return false
	}
	s.pool.SetToken(acct.Name, cred)
	log.Printf("[qwen2api] 账号 %s 凭据已自动续期（signin）", acct.Name)
	return true
}

// noteFailure 按错误分类处置账号。
func (s *Server) noteFailure(acct *pool.Account, err error) {
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		s.pool.MarkAuth(acct, err.Error())
	case upstream.KindQuota:
		s.pool.MarkQuota(acct, err.Error())
	case upstream.KindRateLimit, upstream.KindRisk:
		retry := 0
		var ue *upstream.Error
		if errors.As(err, &ue) {
			retry = ue.RetryAfter
		}
		s.pool.MarkRateLimit(acct, err.Error(), retry)
	default:
		// 其它错误不改账号状态（可能是模型名写错/请求体问题），只记日志。
	}
}

// ---------- 流式 ----------

func (s *Server) serveChatStream(w http.ResponseWriter, r *http.Request, sd *opened,
	req ChatRequest, model string, entry *reqlog.Entry, start time.Time) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		entry.Status = http.StatusInternalServerError
		entry.Error = "响应写入器不支持流式"
		s.log.Add(*entry)
		writeError(w, http.StatusInternalServerError, "internal_error", "响应写入器不支持流式")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	entry.Status = http.StatusOK
	id := "chatcmpl-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	created := time.Now().Unix()

	// 首帧：只给 role，正文增量随后。
	s.writeChunk(w, flusher, ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []StreamChoice{{Index: 0, Delta: Delta{Role: "assistant"}}},
	})

	var (
		usage        *upstream.Usage
		chars        int
		reasoningLen int
		firstByte    bool
		sawUnknown   bool
		streamErr    error
	)
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			streamErr = err
			break
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.UnknownPhase != "" && !sawUnknown {
			sawUnknown = true
			log.Printf("[qwen2api] 上游出现未处理的 phase=%q（产物可能被丢弃，见 docs/UPSTREAM.md §4）", ev.UnknownPhase)
		}
		if ev.Reasoning != "" && s.cfg.ReasoningEnabled() {
			if !firstByte {
				entry.FirstByteMS = time.Since(start).Milliseconds()
				firstByte = true
			}
			reasoningLen += len([]rune(ev.Reasoning))
			s.writeChunk(w, flusher, ChatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{ReasoningContent: ev.Reasoning}}},
			})
		}
		if ev.Text != "" {
			if !firstByte {
				entry.FirstByteMS = time.Since(start).Milliseconds()
				firstByte = true
			}
			chars += len([]rune(ev.Text))
			s.writeChunk(w, flusher, ChatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []StreamChoice{{Index: 0, Delta: Delta{Content: ev.Text}}},
			})
		}
		if ev.Done {
			break
		}
	}

	// 收尾：流内错误要响亮地下发（不能静默当正常结束）。
	if streamErr != nil {
		entry.Error = streamErr.Error()
		if r.Context().Err() == nil {
			s.writeSSE(w, flusher, ErrorBody{Error: ErrorDetail{
				Message: streamErr.Error(),
				Type:    errTypeFor(streamErr),
			}})
		}
	} else if chars == 0 && reasoningLen == 0 {
		// 一字未出（含思考）：形态不符，必须让客户端知道，而不是回一个空回复。
		entry.Error = upstream.ErrNoText.Error()
		log.Printf("[qwen2api] 上游零文本，原始帧取证（%d 条）：%v", len(sd.stream.RawFrames()), sd.stream.RawFrames())
		s.writeSSE(w, flusher, ErrorBody{Error: ErrorDetail{
			Message: upstream.ErrNoText.Error(),
			Type:    "upstream_error",
		}})
	}

	// 正常收尾（或错误后也发 finish，保证客户端能闭合）。
	stop := "stop"
	s.writeChunk(w, flusher, ChatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: &stop}},
	})
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage && usage != nil {
		s.writeChunk(w, flusher, ChatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []StreamChoice{}, Usage: toUsage(usage),
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	entry.Chars = chars
	if usage != nil {
		entry.PromptTokens = usage.InputTokens
		entry.OutputTokens = usage.OutputTokens
	}
	entry.DurationMS = time.Since(start).Milliseconds()
	s.log.Add(*entry)
}

// ---------- 非流式 ----------

func (s *Server) serveChatOnce(w http.ResponseWriter, r *http.Request, sd *opened,
	req ChatRequest, model string, entry *reqlog.Entry, start time.Time) {

	var (
		sb         strings.Builder
		reasoning  strings.Builder
		usage      *upstream.Usage
		sawUnknown bool
	)
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			// 还没写过响应头 ⇒ 可以回正经错误码。
			entry.Status = httpStatusFor(err)
			entry.Error = err.Error()
			entry.DurationMS = time.Since(start).Milliseconds()
			s.log.Add(*entry)
			writeError(w, entry.Status, errTypeFor(err), err.Error())
			return
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.UnknownPhase != "" && !sawUnknown {
			sawUnknown = true
			log.Printf("[qwen2api] 上游出现未处理的 phase=%q（产物可能被丢弃，见 docs/UPSTREAM.md §4）", ev.UnknownPhase)
		}
		if ev.Reasoning != "" && s.cfg.ReasoningEnabled() {
			reasoning.WriteString(ev.Reasoning)
		}
		if ev.Text != "" {
			sb.WriteString(ev.Text)
		}
		if ev.Done {
			break
		}
	}

	content := sb.String()
	if content == "" && reasoning.Len() == 0 {
		entry.Status = http.StatusBadGateway
		entry.Error = upstream.ErrNoText.Error()
		log.Printf("[qwen2api] 上游零文本，原始帧取证（%d 条）：%v", len(sd.stream.RawFrames()), sd.stream.RawFrames())
		entry.DurationMS = time.Since(start).Milliseconds()
		s.log.Add(*entry)
		writeError(w, http.StatusBadGateway, "upstream_error", upstream.ErrNoText.Error())
		return
	}

	stop := "stop"
	resp := ChatResponse{
		ID:      "chatcmpl-" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []Choice{{
			Index:        0,
			Message:      &ResponseMessage{Role: "assistant", Content: content, ReasoningContent: reasoning.String()},
			FinishReason: &stop,
		}},
		Usage: toUsage(usage),
	}
	writeJSON(w, http.StatusOK, resp)

	entry.Status = http.StatusOK
	entry.Chars = len([]rune(content))
	entry.FirstByteMS = time.Since(start).Milliseconds()
	if usage != nil {
		entry.PromptTokens = usage.InputTokens
		entry.OutputTokens = usage.OutputTokens
	}
	entry.DurationMS = time.Since(start).Milliseconds()
	s.log.Add(*entry)
}

// writeChunk 写一个 OpenAI SSE 块。
func (s *Server) writeChunk(w http.ResponseWriter, f http.Flusher, c ChatChunk) {
	s.writeSSE(w, f, c)
}

func (s *Server) writeSSE(w http.ResponseWriter, f http.Flusher, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(raw)
	_, _ = io.WriteString(w, "\n\n")
	f.Flush()
}

// toUsage 上游 usage → OpenAI usage（字段名不同，别直接透传）。
func toUsage(u *upstream.Usage) *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = u.InputTokens + u.OutputTokens
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens}
	}
	return out
}

// httpStatusFor 上游错误 → 对外 HTTP 状态码。
func httpStatusFor(err error) int {
	if errors.Is(err, errNoAccount) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, upstream.ErrQuota) || errors.Is(err, errImageQuotaLocal) ||
		errors.Is(err, errVideoQuotaLocal) || errors.Is(err, errCreditsShort) {
		// 额度耗尽是"现在不行、等下/升级才行" ⇒ 429 而不是 502。
		return http.StatusTooManyRequests
	}
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		// 是网关的上游凭据坏了，不是调用方的 key 错了 ⇒ 502。
		return http.StatusBadGateway
	case upstream.KindQuota, upstream.KindRateLimit:
		return http.StatusTooManyRequests
	case upstream.KindBadInput:
		return http.StatusBadRequest
	case upstream.KindServer, upstream.KindTransport:
		return http.StatusBadGateway
	}
	return http.StatusBadGateway
}

// errTypeFor 上游错误 → OpenAI error.type。
func errTypeFor(err error) string {
	if errors.Is(err, errNoAccount) {
		return "service_unavailable"
	}
	if errors.Is(err, upstream.ErrQuota) {
		return "insufficient_quota"
	}
	if errors.Is(err, errImageQuotaLocal) || errors.Is(err, errVideoQuotaLocal) ||
		errors.Is(err, errCreditsShort) {
		return "insufficient_quota"
	}
	switch upstream.KindOf(err) {
	case upstream.KindAuth:
		return "upstream_auth_error"
	case upstream.KindQuota:
		return "insufficient_quota"
	case upstream.KindRateLimit, upstream.KindRisk:
		return "rate_limit_error"
	case upstream.KindBadInput:
		return "invalid_request_error"
	}
	return "upstream_error"
}

func truncateUA(ua string) string {
	if len(ua) > 120 {
		return ua[:120]
	}
	return ua
}

var _ = fmt.Sprintf
