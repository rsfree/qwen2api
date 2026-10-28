// Package upstream 是 chat.qwen.ai 网页端契约的唯一实现处：
// 请求头、请求体、SSE 判读、错误归类。
//
// 契约真源 = docs/UPSTREAM.md（2026-10-28 一手抓包）；改这里之前先看那份文档。
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// Client 上游 HTTP 客户端（并发安全，进程内单例）。
type Client struct {
	cfg  config.Upstream
	base string
	http *http.Client
}

// New 构造客户端。连接池按"多账号并发"调优：每主机 32 空闲连接，
// 否则流式请求会互相排队等连接。
func New(cfg config.Upstream) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if cfg.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 显式调试开关
	}
	return &Client{
		cfg:  cfg,
		base: strings.TrimRight(cfg.BaseURL, "/"),
		// 不设 Client.Timeout：流式响应可能持续数分钟，总超时由各调用的
		// context 控制（connect 超时在 Dialer 上）。
		http: &http.Client{Transport: transport},
	}
}

// Config 返回上游配置副本。
func (c *Client) Config() config.Upstream { return c.cfg }

// NewChat `POST /api/v2/chats/new` —— 建会话，返回 chat_id。
//
// 会话可长期复用；网关默认每次请求新建（无状态，避免多客户端串味）。
func (c *Client) NewChat(ctx context.Context, cred, model string) (string, error) {
	return c.NewChatTyped(ctx, cred, model, "t2t")
}

// NewChatTyped 同上，可指定会话类型（t2i / t2v 与 t2t 是不同的会话类型）。
func (c *Client) NewChatTyped(ctx context.Context, cred, model, chatType string) (string, error) {
	body, err := json.Marshal(BuildNewChatTyped(model, chatType, time.Now()))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v2/chats/new", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeaders(cred, c.base+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", transportErr("chats/new", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", transportErr("chats/new 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "chats/new")
	if err != nil {
		return "", err
	}
	// decodeEnvelope 已剥掉 {success,request_id,data} 外壳，这里拿到的是 {"id":"…"}。
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", NewErr(KindUnknown, "chats/new", resp.StatusCode, "", "响应不是预期 JSON: "+string(payload), false)
	}
	id := strings.TrimSpace(out.ID)
	if id == "" {
		return "", NewErr(KindUnknown, "chats/new", resp.StatusCode, "", "未返回 data.id", false)
	}
	return id, nil
}

// OpenChat `POST /api/v2/chat/completions?chat_id=` —— 打开流式响应。
//
// 🔴 请求建立阶段（状态码 / x-actual-status-code / WAF / 风控）在这里就判完：
// 失败立刻返回错误，调用方还能回正经 HTTP 状态；进入迭代后的失败只能走流内错误。
func (c *Client) OpenChat(ctx context.Context, cred string, body SubmitBody) (*Stream, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/api/v2/chat/completions?chat_id=%s", c.base, body.ChatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(cred, c.chatReferer(body.ChatID))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("chat/completions", err)
	}
	actual := actualStatus(resp)
	if resp.StatusCode >= 400 || actual >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, classifyHTTP(actual, resp.StatusCode, "chat/completions", snippet)
	}

	// 🔴 必须校验 Content-Type：上游"看起来 200"时可能是 JSON 业务错误
	// （success:false / 空 body）。不校验就会把它当 SSE 读，最后报成
	// "未提取到任何文本" —— 真实原因被彻底掩盖，这是最难查的一类故障。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if _, err := decodeEnvelope(resp, snippet, "chat/completions"); err != nil {
			return nil, err
		}
		return nil, NewErr(KindUnknown, "chat/completions", actual, "",
			"上游返回的不是 SSE（content-type="+ct+"）："+truncate(string(snippet), 300), false)
	}
	return &Stream{
		resp:   resp,
		body:   resp.Body,
		frames: &SSEFrames{},
		parser: NewParser(),
		buf:    make([]byte, 16<<10),
	}, nil
}

// SubmitVideo `POST /api/v2/chat/completions?chat_id=`（stream:false）—— 同步返回 task_id。
//
// 🔴 视频不能走流式：上游对 t2v 只接受 stream:false，同步回一个 tsk_… 任务 id，
// 产物要用 TaskStatus 轮询。这点与文本/图片（SSE 增量）是**两条不同的读法**。
func (c *Client) SubmitVideo(ctx context.Context, cred string, body SubmitBody) (taskID string, err error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/api/v2/chat/completions?chat_id=%s", c.base, body.ChatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeaders(cred, c.chatReferer(body.ChatID))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", transportErr("chat/completions(video)", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", transportErr("chat/completions(video) 读响应", err)
	}
	data, err := decodeEnvelope(resp, payload, "chat/completions(video)")
	if err != nil {
		return "", err
	}
	// 任务 id 藏在 data.messages[0].extra.wanx.task_id（也可能直接给 data.task_id）。
	var out struct {
		TaskID   string `json:"task_id"`
		Messages []struct {
			Extra struct {
				Wanx struct {
					TaskID string `json:"task_id"`
				} `json:"wanx"`
			} `json:"extra"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", NewErr(KindUnknown, "视频提交", resp.StatusCode, "",
			"响应不是预期 JSON: "+truncate(string(data), 300), false)
	}
	if out.TaskID != "" {
		return out.TaskID, nil
	}
	for _, m := range out.Messages {
		if m.Extra.Wanx.TaskID != "" {
			return m.Extra.Wanx.TaskID, nil
		}
	}
	// 🔴 把上游原文带上。2026-10-29 实测：账号积分不足时上游仍回 HTTP 200 +
	// success:true，只是 messages 里没有 wanx.task_id —— 不带原文就只剩
	// "没有 task_id"这句废话，分不清是积分不足、模型不支持还是形态又变了。
	// 原始响应进日志：形态一变，错误信息里能塞的信息总是不够，
	// 而日志里留着全文就能直接对新形态。2026-10-29 实测踩过这一条。
	log.Printf("[qwen2api] 视频提交无 task_id，上游原始响应（前 600 字符）: %s",
		truncate(string(payload), 600))
	hint := ""
	// 注意吃的是 data（已剥掉 {success,data} 外壳），不是 payload。
	if msg := extractMessageText(data); msg != "" {
		hint = "；上游原文：" + truncate(msg, 200)
		// 🔴 积分不足也是「伪装成正常回复」下发的（HTTP 200 + success:true，
		// messages[0].content 里写「Credits 不足 本次生成需要 30 Credits，当前可用 10 Credits」）。
		// 这与流式图片的 extra.update_member 是同一个套路，只是投递通道不同。
		// 不识别它 → 报成"响应里没有 task_id"，把「该充值」误导成「上游改形态了」。
		if isVideoCreditShortage(msg) {
			return "", NewErr(KindQuota, "视频提交", resp.StatusCode, "", msg, true)
		}
	}
	return "", NewErr(KindUnknown, "视频提交", resp.StatusCode, "",
		"响应里没有 task_id（期望 data.messages[0].extra.wanx.task_id）"+hint, false)
}

// isVideoCreditShortage 判断上游那条"积分不足"的回复。
//
// 实测原文：「Credits 不足\n本次生成需要 30 Credits，当前可用 10 Credits。…」
// 只认明确措辞，避免把模型正常解释误判。
func isVideoCreditShortage(msg string) bool {
	if strings.Contains(msg, "Credits 不足") || strings.Contains(msg, "积分不足") {
		return true
	}
	return strings.Contains(msg, "Credits") && strings.Contains(msg, "不足")
}

// extractMessageText 从任务响应里捞出可读文本（错误提示通常塞在 messages[].content）。
func extractMessageText(payload []byte) string {
	var v struct {
		Messages []struct {
			Content string `json:"content"`
			Extra   struct {
				Wanx struct {
					Error string `json:"error"`
					Msg   string `json:"message"`
				} `json:"wanx"`
			} `json:"extra"`
		} `json:"messages"`
	}
	if json.Unmarshal(payload, &v) != nil {
		return ""
	}
	for _, m := range v.Messages {
		if m.Extra.Wanx.Error != "" {
			return m.Extra.Wanx.Error
		}
		if m.Extra.Wanx.Msg != "" {
			return m.Extra.Wanx.Msg
		}
		if s := strings.TrimSpace(m.Content); s != "" {
			return s
		}
	}
	return ""
}

// TaskStatus `GET /api/v2/task/status/<task_id>` —— 视频任务查询。
//
// 返回体原样给出（含 task_status / content / url 等），由上层翻译成 OpenAI 的 videos 形态；
// 本层不解释状态语义（上游的六态是它的定义，不是我们的）。
func (c *Client) TaskStatus(ctx context.Context, cred, taskID string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/v2/task/status/"+taskID, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(cred, c.base+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("task/status", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, transportErr("task/status 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "task/status")
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, NewErr(KindUnknown, "task/status", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	return out, nil
}

// Stream 一条上游 SSE 流的读取器。
type Stream struct {
	resp   *http.Response
	body   io.ReadCloser
	frames *SSEFrames
	parser *Parser
	buf    []byte
	// pending 已切出、待解析的 data 帧。
	pending [][]byte
	// LastEventID 便于日志关联上游 response_id。
	LastEventID string
	// rawFrames 最近若干条原始帧（截断），只在"一字未出"时用于取证。
	rawFrames []string
}

// RawFrames 返回最近原始帧副本（排查形态漂移用）。
func (s *Stream) RawFrames() []string {
	out := make([]string, len(s.rawFrames))
	copy(out, s.rawFrames)
	return out
}

func (s *Stream) keepRaw(data []byte) {
	const maxKeep = 6
	txt := string(data)
	if len(txt) > 400 {
		txt = txt[:400] + "…"
	}
	s.rawFrames = append(s.rawFrames, txt)
	if len(s.rawFrames) > maxKeep {
		s.rawFrames = s.rawFrames[len(s.rawFrames)-maxKeep:]
	}
}

// traceSSE 由 QWEN2API_TRACE_SSE=1 打开：把每一帧原文打到 stderr。
//
// 为什么要有：上游加新 phase（比如 image_gen_tool）时，解析器会静默跳过它，
// 表象是"没输出"或"少了一段"。这时候唯一的办法是看原文。
func traceSSE(data []byte) {
	if os.Getenv("QWEN2API_TRACE_SSE") == "" {
		return
	}
	txt := string(data)
	if len(txt) > 2000 {
		txt = txt[:2000] + "…"
	}
	fmt.Fprintf(os.Stderr, "[sse] %s\n", txt)
}

// Next 读取下一个有效事件。ok=false 且 err=nil 表示流正常结束。
func (s *Stream) Next() (Event, bool, error) {
	for {
		if len(s.pending) > 0 {
			data := s.pending[0]
			s.pending = s.pending[1:]
			s.keepRaw(data)
			traceSSE(data)
			ev, ok, err := s.parser.Parse(data)
			if err != nil {
				return Event{}, false, err
			}
			if !ok {
				continue
			}
			if ev.Created != nil {
				s.LastEventID = ev.Created.ResponseID
			}
			return ev, true, nil
		}
		n, readErr := s.body.Read(s.buf)
		if n > 0 {
			s.pending = append(s.pending, s.frames.Push(s.buf[:n])...)
			continue
		}
		switch readErr {
		case nil:
			continue
		case io.EOF:
			s.pending = append(s.pending, s.frames.Flush()...)
			if len(s.pending) == 0 {
				return Event{}, false, nil
			}
		default:
			return Event{}, false, transportErr("chat 流读取", readErr)
		}
	}
}

// Close 释放连接（幂等）。
func (s *Stream) Close() {
	if s.body != nil {
		_ = s.body.Close()
		s.body = nil
	}
}

// ModelEntry 上游模型条目（只取实现需要的字段）。
type ModelEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Info struct {
		IsActive        bool  `json:"is_active"`
		IsVisitorActive bool  `json:"is_visitor_active"`
		CreatedAt       int64 `json:"created_at"`
		Meta            struct {
			Description      string         `json:"description"`
			ShortDescription string         `json:"short_description"`
			MaxContextLength int            `json:"max_context_length"`
			ChatType         []string       `json:"chat_type"`
			Capabilities     map[string]any `json:"capabilities"`
			Modality         []string       `json:"modality"`
		} `json:"meta"`
	} `json:"info"`
}

// ListModels `GET /api/v2/models/` —— 需要凭据（匿名请求被 WAF 404/405）。
//
// 响应是 `{"success":true,"data":{"data":[...]}}` —— 注意是**两层 data**，
// 与旧契约（`data` 直接是数组）不同，2026-10-28 实测。
func (c *Client) ListModels(ctx context.Context, cred string) ([]ModelEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v2/models/", nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.buildHeaders(cred, c.base+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("models", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, transportErr("models 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "models")
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			Data []ModelEntry `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		// 兼容旧形态：data 直接是数组。
		var legacy struct {
			Data []ModelEntry `json:"data"`
		}
		if err2 := json.Unmarshal(payload, &legacy); err2 == nil && len(legacy.Data) > 0 {
			return legacy.Data, nil
		}
		return nil, NewErr(KindUnknown, "models", resp.StatusCode, "", "响应不是预期结构", false)
	}
	return out.Data.Data, nil
}

// AccountInfo 账号资料（面板"验活"用，不含任何凭据原文）。
type AccountInfo struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// WhoAmI `GET auth.qwen.ai/api/v2/auths/` —— 验活：能拿到账号资料即凭据有效。
func (c *Client) WhoAmI(ctx context.Context, cred string) (*AccountInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	base := strings.TrimRight(c.cfg.AuthBaseURL, "/")
	if base == "" {
		base = c.base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v2/auths/", nil)
	if err != nil {
		return nil, err
	}
	// auth 域是跨站端点，头与业务域不同（见 buildHeadersFor）。
	req.Header = c.buildHeadersFor(cred, c.base+"/", base != c.base)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("auths", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, transportErr("auths 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "auths")
	if err != nil {
		return nil, err
	}
	var info AccountInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return nil, NewErr(KindUnknown, "auths", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	if info.ID == "" && info.Email == "" {
		return nil, NewErr(KindAuth, "auths", resp.StatusCode, "", "未返回账号资料（凭据可能已失效）", true)
	}
	return &info, nil
}

// decodeEnvelope 解包 `{success, request_id, data}` 信封并做错误归类。
//
// 失败返回的 payload 是 `data` 字段（成功路径）。
func decodeEnvelope(resp *http.Response, raw []byte, op string) ([]byte, error) {
	actual := actualStatus(resp)
	text := string(raw)
	if strings.Contains(text, "aliyun_waf") {
		return nil, NewErr(KindRisk, op, actual, "", "上游返回 WAF 挑战页（凭据/出口问题）", true)
	}
	if strings.Contains(text, "RGV587") || strings.Contains(text, "FAIL_SYS_USER_VALIDATE") {
		return nil, NewErr(KindRisk, op, actual, "RGV587", "上游 x5sec 风控", true)
	}
	if resp.StatusCode >= 400 || actual >= 400 {
		return nil, classifyHTTP(actual, resp.StatusCode, op, raw)
	}

	var env struct {
		Success   *bool           `json:"success"`
		RequestID string          `json:"request_id"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, NewErr(KindUnknown, op, actual, "", "响应不是 JSON: "+truncate(text, 200), false)
	}
	if env.Success != nil && !*env.Success {
		// 业务失败：data 里带 code/details。
		var biz struct {
			Code    string `json:"code"`
			Details string `json:"details"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &biz)
		}
		return nil, classifyBusiness(op, actual, biz.Code, biz.Details)
	}
	if len(env.Data) == 0 {
		// auths/ 这类端点没有信封，直接返回原始 JSON。
		return raw, nil
	}
	return env.Data, nil
}

// classifyHTTP 按 HTTP 状态归类。
func classifyHTTP(actual, status int, op string, raw []byte) error {
	detail := truncate(string(raw), 300)
	switch {
	case actual == 401 || status == 401:
		return NewErr(KindAuth, op, actual, "Unauthorized", detail, true)
	case actual == 403 || status == 403:
		return NewErr(KindAuth, op, actual, "Forbidden", detail, true)
	case actual == 429 || status == 429:
		return NewErr(KindRateLimit, op, actual, "RateLimited", detail, true)
	case actual == 400 || status == 400:
		return NewErr(KindBadInput, op, actual, "Bad_Request", detail, false)
	case actual == 404 || status == 404:
		return NewErr(KindUnknown, op, actual, "Not_Found", detail, false)
	case actual >= 500 || status >= 500:
		return NewErr(KindServer, op, actual, "", detail, false)
	}
	return NewErr(KindUnknown, op, actual, "", detail, false)
}

// classifyBusiness 按业务码归类。
func classifyBusiness(op string, actual int, code, details string) error {
	low := strings.ToLower(code + " " + details)
	switch {
	case strings.Contains(code, "Unauthorized") || strings.Contains(low, "token has expired") ||
		strings.Contains(low, "please log in"):
		return NewErr(KindAuth, op, actual, code, details, true)
	case strings.Contains(code, "Bad_Request") || strings.Contains(details, "Bad_Request"),
		strings.Contains(low, "requestvalidationerror"):
		return NewErr(KindBadInput, op, actual, code, details, false)
	case strings.Contains(code, "Not_Found"), strings.Contains(low, "chat_not_found"),
		strings.Contains(low, "task not found"):
		return NewErr(KindUnknown, op, actual, code, details, false)
	case code == "RateLimited", strings.Contains(low, "quota_limit"),
		strings.Contains(details, "额度"), strings.Contains(low, "quota"),
		strings.Contains(low, "积分"), strings.Contains(low, "credits"),
		strings.Contains(low, "insufficient"), strings.Contains(low, "not enough"),
		strings.Contains(low, "余额"):
		// 未受理 ⇒ 未计费 ⇒ 换号重试安全。
		return NewErr(KindQuota, op, actual, code, details, true)
	}
	return NewErr(KindUnknown, op, actual, code, details, false)
}

// transportErr 网络层错误归类：可能已受理 ⇒ 不可重试。
func transportErr(op string, err error) error {
	detail := err.Error()
	kind := KindTransport
	if strings.Contains(detail, "context deadline exceeded") || strings.Contains(detail, "timeout") {
		kind = KindTransport
	}
	return NewErr(kind, op, 0, "", detail, false)
}
