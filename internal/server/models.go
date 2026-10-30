package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ModelInfo `/v1/models` 条目：OpenAI 四个原生字段 + 加性扩展。
//
// 扩展字段（title/media/thinking/vision/verified/notes）对严格客户端无害 ——
// 只取 id/object/created/owned_by 的客户端照样能用。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Title   string `json:"title,omitempty"`
	Media   string `json:"media,omitempty"`
	// Thinking 上游是否支持思考档（false ⇒ fast 之外的两档无意义）。
	Thinking bool `json:"thinking"`
	Vision   bool `json:"vision"`
	// Verified 是否已端到端实测跑通本网关链路。
	Verified bool   `json:"verified"`
	Notes    string `json:"notes,omitempty"`
	Context  int    `json:"context_length,omitempty"`
	// Endpoint 调用该模型该走的端点。生成模型（图片/视频）不能走 chat，
	// 这个字段就是给客户端看的"该往哪发"。文本模型留空 = /v1/chat/completions。
	Endpoint string `json:"endpoint,omitempty"`
	// Params 该模型的可选参数（尺寸 / 分辨率 / 档位单价），只有生成模型有。
	Params map[string]any `json:"params,omitempty"`
}

// builtinModels 内置清单 = 2026-10-28 实测 `GET /api/v2/models/` 的 7 个 t2t 模型。
//
// 为什么要内置：上游清单要凭据才能拉；网关冷启动/全号失效时 `/v1/models`
// 不能空着（空清单会被客户端当成"服务没模型"）。有号时以线上为准。
var builtinModels = []upstream.ModelEntry{
	model("qwen3.7-plus", "Qwen3.7-Plus", true, true, 1000000),
	model("qwen3.8-max", "Qwen3.8-Max", true, true, 1000000),
	model("qwen3.8-omni-flash", "Qwen3.8-Omni-Flash", true, true, 1000000),
	model("qwen3.7-max", "Qwen3.7-Max", true, false, 1000000),
	model("qwen3.6-plus", "Qwen3.6-Plus", true, true, 1000000),
	model("qwen3.5-plus", "Qwen3.5-Plus", true, true, 1000000),
	model("qwen3.5-omni-plus", "Qwen3.5-Omni-Plus", false, true, 262144),
}

func model(id, name string, thinking, vision bool, ctx int) upstream.ModelEntry {
	var e upstream.ModelEntry
	e.ID = id
	e.Name = name
	e.Info.IsActive = true
	e.Info.Meta.ShortDescription = name
	e.Info.Meta.MaxContextLength = ctx
	e.Info.Meta.ChatType = []string{"t2t"}
	e.Info.Meta.Capabilities = map[string]any{"thinking": thinking, "vision": vision}
	return e
}

// ModelCache 上游模型清单的 TTL 缓存。
//
// 纪律：**绝不让 /v1/models 因为上游抖动而 5xx** —— 能力探测挂了会被误读成
// "服务不可用"。拉取失败就沿用上一份好清单（首拉失败用内置清单）。
type ModelCache struct {
	mu        sync.RWMutex
	entries   []upstream.ModelEntry
	expiresAt time.Time
	lastErr   string
	fetches   int
	ttl       time.Duration

	up   *upstream.Client
	pool *pool.Pool
}

// NewModelCache 构造缓存。
func NewModelCache(up *upstream.Client, p *pool.Pool) *ModelCache {
	return &ModelCache{up: up, pool: p, ttl: 10 * time.Minute, entries: builtinModels}
}

// List 返回 OpenAI 形态清单。
func (c *ModelCache) List(ctx context.Context) []ModelInfo {
	c.refresh(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelInfo, 0, len(c.entries))
	for _, e := range c.entries {
		if !e.Info.IsActive {
			continue
		}
		// 只列能跑 t2t 的：上游 chat_type 不含 t2t 的一律不注册（列出来就是假能力）。
		ok := len(e.Info.Meta.ChatType) == 0
		for _, t := range e.Info.Meta.ChatType {
			if t == "t2t" {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		thinking, _ := e.Info.Meta.Capabilities["thinking"].(bool)
		vision, _ := e.Info.Meta.Capabilities["vision"].(bool)
		out = append(out, ModelInfo{
			ID:       e.ID,
			Object:   "model",
			Created:  e.Info.CreatedAt,
			OwnedBy:  "qwen",
			Title:    e.Name,
			Media:    "text",
			Thinking: thinking,
			Vision:   vision,
			Verified: true,
			Context:  e.Info.Meta.MaxContextLength,
			Notes:    "经 qwen2api 转发；支持 thinking 参数（auto/thinking/fast）。",
		})
	}
	return out
}

// refresh 到期后拉一次上游；失败沿用旧清单并顺延 TTL（负缓存，不每次打上游）。
func (c *ModelCache) refresh(ctx context.Context) {
	c.mu.RLock()
	fresh := time.Now().Before(c.expiresAt)
	c.mu.RUnlock()
	if fresh {
		return
	}

	acct, ok := c.pool.Pick()
	if !ok {
		c.mu.Lock()
		c.expiresAt = time.Now().Add(time.Minute) // 短暂负缓存：没号就别反复试
		c.mu.Unlock()
		return
	}
	entries, err := c.up.ListModels(ctx, acct.Token)
	c.pool.Release(acct)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetches++
	if err != nil || len(entries) == 0 {
		if err != nil {
			c.lastErr = err.Error()
		}
		c.expiresAt = time.Now().Add(c.ttl)
		return
	}
	c.entries = entries
	c.lastErr = ""
	c.expiresAt = time.Now().Add(c.ttl)
}

// Info 观测面（不含清单内容）。
func (c *ModelCache) Info() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]any{
		"cached":     len(c.entries),
		"expires_in": int(time.Until(c.expiresAt).Seconds()),
		"fetches":    c.fetches,
		"last_error": c.lastErr,
	}
}

// handleModels `GET /v1/models`。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	// 文本模型来自上游缓存，图片/视频模型由本网关补上（见 mediaModels）。
	list := s.modelList(r.Context())
	if len(list) == 0 {
		// 理论上不会发生（内置清单兜底）；真发生了也要给合法空清单而不是 500。
		list = []ModelInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": list})
}

// Known 模型是否在清单里（用于提前拒绝乱写的模型名，避免白跑一次上游）。
func (c *ModelCache) Known(ctx context.Context, id string) bool {
	for _, m := range c.List(ctx) {
		if m.ID == id {
			return true
		}
	}
	return false
}
