package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrImageNotFinalized 会话历史里已经有图，但链接还没被 CDN 签名 —— 现在下载必 404。
//
// 这是"上游确实画了、只是慢"的铁证：调用方拿不到图不是**失败**，是**晚**。
// 上层据此能讲清"别重画、几分钟后可以取回"，而不是笼统报一句超时。
var ErrImageNotFinalized = errors.New("会话历史里的图片链接尚未定稿（CDN 未签名，下载必 404）")

// historyTimeout 会话历史是只读小请求（实测 0.25s / 9KB），20s 足够。
//
// 它只在**产物已经悬空**时才被调用，不能让它把一次失败拖成两倍等待。
const historyTimeout = 20 * time.Second

// LatestChatImages 取会话里"最新一条带图回复"的图片直链。
//
// 端点（2026-10-31 实测，raw curl：HTTP 200 / 9.4KB）：
//
//	GET /api/v2/chats/<chat_id>?direction=up&limit=10
//	→ data.chat.history.messages   // **map**，键是消息 id，不是数组
//
// 🔴 图片在 assistant 消息的 `content_list[].content`（纯 URL），
// 与 SSE 的 `extra.image_list[].image` 是**两种形态**。只认 SSE 那套会漏，
// 所以两处都收（`tool_result` 也顺带收，上游偶发只给其一）。
//
// 只取**最新一条**带图回复：会话是"取出即用"的空会话，正常只有本轮；
// 万一被复用，往前翻会把上一轮的图当成本次产物 —— 那比没有更糟。
func (c *Client) LatestChatImages(ctx context.Context, cred, chatID string) ([]string, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, fmt.Errorf("chat_id 为空")
	}
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/api/v2/chats/%s?direction=up&limit=10", c.base, chatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// referer 必须是会话页：只读端点同样按它做权限/防盗链判定（实测）。
	req.Header = c.buildHeaders(cred, c.chatReferer(chatID))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("chats/历史", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, transportErr("chats/历史 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "chats/历史")
	if err != nil {
		return nil, err
	}

	var out struct {
		Chat struct {
			History struct {
				Messages map[string]historyMessage `json:"messages"`
			} `json:"history"`
		} `json:"chat"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, NewErr(KindUnknown, "chats/历史", resp.StatusCode, "",
			"响应不是预期结构: "+truncate(string(payload), 200), false)
	}

	var best *historyMessage
	for _, m := range out.Chat.History.Messages {
		if m.Role != "assistant" || len(m.imageURLs()) == 0 {
			continue
		}
		// 取 timestamp 最大的一条。并列时后者胜（map 顺序随机，无所谓）。
		if best == nil || m.Timestamp >= best.Timestamp {
			mm := m
			best = &mm
		}
	}
	if best == nil {
		return nil, nil
	}
	// 🔴 只交付**已定稿**的链接（2026-11-02 对线上 chat fb709dd6 实测）：
	// 落库分两步，"content_list 里出现地址" ≠ "现在能下载"。未定稿形态
	// `.../t2i/<msg>/1793612603855`（无扩展名、无签名）下回来是 404 + 226 字节
	// Tengine HTML —— 和"刚出图还没转存完"的表象一模一样，极易误诊。
	// 定稿后才变成 `.../1793612603855e.png?key=<JWT>`，实测要再等 2~6 分钟。
	//
	// 宁可报"未定稿"也别把死链当产物交出去：调用方拿到 200 + 打不开的 URL
	// 会以为成功并把它存下来，这比直接 504 糟得多。
	final, pending := splitFinalized(best.imageURLs())
	if len(final) > 0 {
		return final, nil
	}
	return nil, fmt.Errorf("%w（%d 个链接待签名）", ErrImageNotFinalized, len(pending))
}

// splitFinalized 把链接分成"现在就能下"与"还在等 CDN 签名"两组（保序）。
func splitFinalized(urls []string) (final, pending []string) {
	for _, u := range urls {
		if finalizedImageURL(u) {
			final = append(final, u)
		} else {
			pending = append(pending, u)
		}
	}
	return final, pending
}

// finalizedImageURL 该链接是否已定稿（现在下载能拿到真图）。
//
// 🔴 判据只用 `?key=` —— 依据是同一条会话间隔几分钟抓两次的对照：
//
//	未定稿 `.../1793612603855`           → CDN 404 + 226B Tengine HTML
//	定稿   `.../1793612603855e.png?key=` → 200 / image/png / 9.3MB
//
// 不拿 `status:"finished"` 或 `done` 当判据：那条会话两者都已是完成态，
// 链接却还没签名 —— 它们只说明"生成结束"，不说明"文件可下载"。
//
// fail-closed 的取舍：哪天上游改成公开直链（无 key），这里会把真产物误判成
// 未定稿，表现是"补捞永远无果、仍报超时"—— 也就是今天的行为，不会更坏。
func finalizedImageURL(u string) bool {
	_, query, ok := strings.Cut(u, "?")
	return ok && strings.Contains(query, "key=")
}

// historyMessage 会话历史里的单条消息（只声明实现用到的字段）。
type historyMessage struct {
	Role        string  `json:"role"`
	ChatType    string  `json:"chat_type"`
	Timestamp   float64 `json:"timestamp"`
	ContentList []struct {
		Content string `json:"content"`
		Phase   string `json:"phase"`
		Status  string `json:"status"`
	} `json:"content_list"`
	Extra struct {
		ImageList []struct {
			Image string `json:"image"`
		} `json:"image_list"`
		ToolResult []struct {
			Image string `json:"image"`
		} `json:"tool_result"`
	} `json:"extra"`
}

// imageURLs 该消息产出的图片直链（多形态合并去重，保序）。
func (m historyMessage) imageURLs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		// 🔴 只认 http(s)：content_list[].content 里也可能放正文，
		// 不过滤就会把模型说的一段话当成"图片 URL"回给调用方。
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, e := range m.ContentList {
		add(e.Content)
	}
	for _, e := range m.Extra.ImageList {
		add(e.Image)
	}
	for _, e := range m.Extra.ToolResult {
		add(e.Image)
	}
	return out
}
