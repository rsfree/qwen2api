package upstream

import (
	"fmt"
	"time"
)

// SubmitVersion 请求体里的 version 字段（≠ HTTP 头 version）。
// 实测 2026-10-28 抓包恒为 "2.1"；header version 才是前端版本号 0.3.26。
const SubmitVersion = "2.1"

// FeatureConfig 思考档位配置 —— 三档与前端 UI「自动 / 思考 / 快速」逐字对齐。
//
// 实测（2026-10-28）auto 档完整形态：
//
//	thinking_enabled:true, output_schema:"phase", research_mode:"normal",
//	auto_thinking:true, thinking_mode:"Auto", thinking_format:"summary", auto_search:true
//
// 「快速」档是唯一 thinking_enabled=false 的形态（关思考、首字最快），
// 且**不带 thinking_format**（前端也不带）。
type FeatureConfig struct {
	ThinkingEnabled bool   `json:"thinking_enabled"`
	OutputSchema    string `json:"output_schema"`
	ResearchMode    string `json:"research_mode"`
	AutoThinking    bool   `json:"auto_thinking"`
	ThinkingMode    string `json:"thinking_mode"`
	ThinkingFormat  string `json:"thinking_format,omitempty"`
	AutoSearch      bool   `json:"auto_search"`
}

// Gears 三档思考配置（返回副本，调用方改不动包级状态）。
var gears = map[string]FeatureConfig{
	"auto": {
		ThinkingEnabled: true, OutputSchema: "phase", ResearchMode: "normal",
		AutoThinking: true, ThinkingMode: "Auto", ThinkingFormat: "summary", AutoSearch: true,
	},
	"thinking": {
		ThinkingEnabled: true, OutputSchema: "phase", ResearchMode: "normal",
		AutoThinking: false, ThinkingMode: "Thinking", ThinkingFormat: "summary", AutoSearch: true,
	},
	"fast": {
		ThinkingEnabled: false, OutputSchema: "phase", ResearchMode: "normal",
		AutoThinking: false, ThinkingMode: "Fast", AutoSearch: true,
	},
}

// GearConfig 取档位配置；未知档位回落 auto（与 config.Gear 同口径）。
func GearConfig(gear string) FeatureConfig {
	if c, ok := gears[gear]; ok {
		return c
	}
	return gears["auto"]
}

// NewChatBody `POST /api/v2/chats/new` 请求体。
type NewChatBody struct {
	Title     string   `json:"title"`
	Models    []string `json:"models"`
	ChatMode  string   `json:"chat_mode"`
	ChatType  string   `json:"chat_type"`
	Timestamp int64    `json:"timestamp"` // 毫秒
	ProjectID string   `json:"project_id"`
}

// BuildNewChat 构造建会话请求体。
func BuildNewChat(model string, now time.Time) NewChatBody {
	return BuildNewChatTyped(model, "t2t", now)
}

// BuildNewChatTyped 同上，可指定会话类型（t2i/t2v 会话与 t2t 分开）。
func BuildNewChatTyped(model, chatType string, now time.Time) NewChatBody {
	if chatType == "" {
		chatType = "t2t"
	}
	return NewChatBody{
		Title:     "New Chat",
		Models:    []string{model},
		ChatMode:  "normal",
		ChatType:  chatType,
		Timestamp: now.UnixMilli(),
		ProjectID: "",
	}
}

// Message 上游 messages[0] 形态（逐字对齐抓包，键名不可改）。
//
// 🔴 三个易错点：
//  1. `id` 必须存在且为 null（不是省略）—— 上游按字段存在性校验；
//  2. `chatId` 与 `chat_id` **双写**，缺小写直接 400 RequestValidationError；
//  3. `files` 恒在（纯文本为空数组 `[]`，不是 null）。
type Message struct {
	ID            *string        `json:"id"`
	Fid           string         `json:"fid"`
	ParentID      *string        `json:"parentId"`
	ChildrenIDs   []string       `json:"childrenIds"`
	Role          string         `json:"role"`
	Content       string         `json:"content"`
	UserAction    string         `json:"user_action"`
	Files         []FileRef      `json:"files"`     // 纯文本恒为 []，不为 null
	Timestamp     int64          `json:"timestamp"` // 秒
	Models        []string       `json:"models"`
	Model         string         `json:"model"`
	ChatType      string         `json:"chat_type"`
	FeatureConfig FeatureConfig  `json:"feature_config"`
	Extra         map[string]any `json:"extra"`
	SubChatType   string         `json:"sub_chat_type"`
	ParentIDSnake *string        `json:"parent_id"`
}

// FileRef 附件条目。
//
// 🔴 形状必须与前端逐字段一致（见 upload.go 的 buildFileEntry）：
// 少 status/greenNet/itemId 会被上游当成"未上传完成"而静默忽略，
// 于是提示词里说的"这张图"变成无图请求 —— 没有报错，最难查。
type FileRef = map[string]any

// SubmitBody `POST /api/v2/chat/completions` 请求体。
type SubmitBody struct {
	Stream            bool      `json:"stream"`
	Version           string    `json:"version"`
	IncrementalOutput bool      `json:"incremental_output"`
	ChatID            string    `json:"chatId"`
	ParentID          string    `json:"parentId"`
	ChatIDSnake       string    `json:"chat_id"`
	ChatMode          string    `json:"chat_mode"`
	Model             string    `json:"model"`
	ParentIDSnake     *string   `json:"parent_id"`
	Messages          []Message `json:"messages"`
	Timestamp         int64     `json:"timestamp"` // 秒
	// Size 画幅（仅 t2i/t2v 用；文本请求不出现该键）。
	Size string `json:"size,omitempty"`
}

// ChatSpec 一次上游对话的"类型规格"。
//
// 🔴 为什么需要它：三个 chat_type 字段**并不总是同值**（2026-10-28 实测）：
//
//	t2t    : chat_type=t2t    sub_chat_type=t2t
//	t2i    : chat_type=t2i    sub_chat_type=t2i        + extra.meta.model=qwen-image-3.0
//	t2v    : chat_type=t2v    sub_chat_type=zaodian_generate_video
//	                          + extra.meta.biz_data（JSON 字符串）+ videoGenerationMode=standard
//
// 把 sub_chat_type 直接等于 chat_type 会让 t2v 静默失败（上游按 sub 分派业务线）。
type ChatSpec struct {
	ChatType    string // t2t / t2i / image_edit / t2v
	SubChatType string // 一般同 ChatType，t2v 例外（zaodian_generate_video）
	Size        string // 画幅：1:1 / 3:4 / 4:3 / 16:9 / 9:16 / auto
	ImageModel  string // t2i：图片模型名（qwen-image-3.0）
	BizData     string // t2v：业务参数 JSON 字符串
	VideoMode   string // t2v：standard
	// Files 附件（image_edit / 文件解析）。每张一项，必须来自 UploadAttachment。
	Files []FileRef
}

// TextSpec 文本对话。
func TextSpec() ChatSpec { return ChatSpec{ChatType: "t2t", SubChatType: "t2t"} }

// ImageSpec 文生图。imageModel 空则用上游默认（不传该键）。
func ImageSpec(imageModel, size string) ChatSpec {
	return ChatSpec{ChatType: "t2i", SubChatType: "t2i", Size: size, ImageModel: imageModel}
}

// ImageEditSpec 图生图 / 图片编辑。
//
// 🔴 三条与 t2i 不同的实测口径（来源 image-adapter §2 的两条链路表）：
//   - `chat_type` / `sub_chat_type` = `image_edit`
//   - 带 `files[]`（每张入图一项）
//   - **不带 `size`**（顶层与 extra.meta 都不带 —— 无实测依据，故不发）
func ImageEditSpec(files []FileRef) ChatSpec {
	return ChatSpec{ChatType: "image_edit", SubChatType: "image_edit", Files: files}
}

// VideoSpec 文生视频。
//
// biz_data 是**字符串化的 JSON**（不是对象）—— 这是上游的形态，写成对象会 Bad_Request。
// 参数取自用户实测抓包：rootModel=happyhorse11 / genMode=vid_gen / resolution=720P /
// duration=5 / audio=true。resolution 与 duration 的合法档位见 docs/UPSTREAM.md §7。
func VideoSpec(size, resolution string, duration int, audio bool, rootModel string) ChatSpec {
	if resolution == "" {
		resolution = "720P"
	}
	if duration <= 0 {
		duration = 5
	}
	if rootModel == "" {
		rootModel = "happyhorse11"
	}
	biz := fmt.Sprintf(
		`{"bizScene":"genVideo","req":{"rootModel":%q,"genMode":"vid_gen","params":{"resolution":%q,"size":%q,"duration":%d,"audio":%t}}}`,
		rootModel, resolution, size, duration, audio)
	return ChatSpec{
		ChatType:    "t2v",
		SubChatType: "zaodian_generate_video",
		Size:        size,
		BizData:     biz,
		VideoMode:   "standard",
	}
}

// BuildSubmit 构造一轮对话提交体。
//
// parentID：新会话传 ""（实测首次对话顶层 parentId 为空串）；复用已有会话时
// 传**上一条消息 id**（2026-10-28 实测的链式形态）。
func BuildSubmit(chatID, parentID, model, prompt string, gear string, now time.Time) SubmitBody {
	return BuildSubmitSpec(chatID, parentID, model, prompt, gear, TextSpec(), now)
}

// BuildSubmitSpec 同上，但带完整类型规格（t2i 的图片模型、t2v 的 biz_data 都在 spec 里）。
//
// 🔴 图片生成**在 t2t 下也能触发**（模型自己决定调 image_gen 工具），但那样
// "画一只猫"有可能只换来一段描述。显式传 t2i + extra.meta.model 才是"一定要图"。
func BuildSubmitSpec(chatID, parentID, model, prompt, gear string, spec ChatSpec, now time.Time) SubmitBody {
	ts := now.Unix()
	if spec.ChatType == "" {
		spec.ChatType = "t2t"
	}
	if spec.SubChatType == "" {
		spec.SubChatType = spec.ChatType
	}
	var parentPtr *string
	if parentID != "" {
		p := parentID
		parentPtr = &p
	}
	files := make([]FileRef, 0, len(spec.Files))
	files = append(files, spec.Files...)
	meta := map[string]any{"subChatType": spec.SubChatType}
	if spec.Size != "" {
		meta["size"] = spec.Size
	}
	if spec.ImageModel != "" {
		meta["model"] = spec.ImageModel
	}
	if spec.BizData != "" {
		meta["biz_data"] = spec.BizData
	}
	if spec.VideoMode != "" {
		meta["videoGenerationMode"] = spec.VideoMode
	}
	msg := Message{
		ID:            nil,
		Fid:           UUID(),
		ParentID:      parentPtr,
		ChildrenIDs:   []string{UUID()},
		Role:          "user",
		Content:       prompt,
		UserAction:    "chat",
		Files:         files,
		Timestamp:     ts,
		Models:        []string{model},
		Model:         "",
		ChatType:      spec.ChatType,
		FeatureConfig: GearConfig(gear),
		Extra:         map[string]any{"meta": meta},
		SubChatType:   spec.SubChatType,
		ParentIDSnake: parentPtr,
	}
	body := SubmitBody{
		Stream: spec.ChatType != "t2v", // 视频必须同步拿 task_id，不能流式

		Version:           SubmitVersion,
		IncrementalOutput: true,
		ChatID:            chatID,
		ParentID:          parentID,
		ChatIDSnake:       chatID,
		ChatMode:          "normal",
		Model:             model,
		ParentIDSnake:     nil,
		Messages:          []Message{msg},
		Timestamp:         ts,
	}
	if spec.Size != "" {
		body.Size = spec.Size
	}
	return body
}
