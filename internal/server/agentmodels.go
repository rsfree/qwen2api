package server

import (
	"sort"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// agentCatalog 网关侧的 Agent 模型目录。
//
// 为什么需要这一层：Agent 模式是**独立模型清单**
// （`GET /api/v2/models/agent_mode`，2026-11-08 实测只有两个，见 docs/UPSTREAM.md §10.2），
// 与普通 `/api/v2/models/` 的 7 个 chat 模型不重叠。
//
// 🔴 不能直接复用上游 id：客户端传 `qwen3.8-max` 的语义是"普通对话"，
// 传 `qwen3.8-max-agent` 才是 Agent。合并成一个 id 会让同一请求的
// chat_type 变得不可预测 —— 那是"看起来能跑、结果随机"的最坏形态。
//
// 另注：Agent 的 `sub_chat_type` 恒为 lite，**与底座模型无关**（§10.1），
// 所以这里只决定"用哪个底座跑 agent"，不改类型位点。
type agentModelEntry struct {
	ID     string // 网关侧模型 id（客户端传这个）
	Base   string // 上游 chat 模型 id（放进 model 字段）
	Title  string
	Vision bool
}

var agentCatalog = []agentModelEntry{
	{ID: "qwen3.8-max-agent", Base: "qwen3.8-max", Title: "Qwen3.8-Max Agent", Vision: true},
	{ID: "qwen3.8-flash-agent", Base: "qwen3.8-flash", Title: "Qwen3.8-Flash Agent", Vision: false},
}

// resolveAgentModel 判断模型名是不是 Agent 模型；是则返回上游底座模型 id。
func resolveAgentModel(model string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(model))
	for _, m := range agentCatalog {
		if m.ID == id {
			return m.Base, true
		}
	}
	return "", false
}

// agentSpecFor 返回该模型对应的上游规格：Agent 模型走 agent_mode，其余走 t2t。
//
// 三个协议前门（chat / messages / responses）都调它 —— 规格只有一处真相，
// 免得哪天只给 chat 加了 Agent，Anthropic 前门却还是 t2t。
func agentSpecFor(model string) (base string, spec upstream.ChatSpec, isAgent bool) {
	if base, ok := resolveAgentModel(model); ok {
		return base, upstream.AgentSpec(), true
	}
	return model, upstream.TextSpec(), false
}

// agentModelList 把 Agent 模型列进 `/v1/models`。
//
// 清单必须与能力一一对应 —— 列一个用不了的名字比不列更糟。这里 capacity 如实标注：
// 只有 max 有视觉，两者都没有思考档位（Agent 在网页端没有档位选择器）。
func (s *Server) agentModelList() []ModelInfo {
	out := make([]ModelInfo, 0, len(agentCatalog))
	for _, m := range agentCatalog {
		out = append(out, ModelInfo{
			ID:       m.ID,
			Object:   "model",
			Created:  mediaModelCreated,
			OwnedBy:  "qwen",
			Title:    m.Title,
			Media:    "text",
			Thinking: false,
			Vision:   m.Vision,
			Verified: true,
			Context:  1000000,
			Notes: "Agent 模式：服务端自主规划并执行工具（bash / write_file 等），" +
				"一次请求内跑完 think→tool_call→answer 整个循环；超时按更长预算放宽。" +
				"思考原文走 reasoning_content。注意：需要澄清时上游会抛 ask_user 交互工具，" +
				"单轮 API 无法回答（表现为一段反问文本）；产物文件不会随响应下发。" +
				"thinking 参数对它无效（上游固定 Fast）。",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
