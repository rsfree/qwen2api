package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
	return best.imageURLs(), nil
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
