package upstream

// 视频上游的读法：状态归一、产物抽取、错误归因、用量抽取。
//
// 🔴 这一层是「转换点 ④/⑥」——上游查询对象 → canonical 任务对象。
// 它必须是**唯一**读上游任务对象的地方：查询、回调推送、列表都从这里取，
// 一旦出现第二套读法，回调体和查询体对不上就是时间问题。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NormalizeVideoStatus 上游 task_status → canonical 六态。
//
// 上游取值域**没有官方文档**（docs/UPSTREAM.md §4.7 只记到 "task_status / content 等"），
// 所以只能按包含关系映射，并且把原文一并带回去（status_raw）——
// 判错时至少能看见原始值，而不是对着一句 "task failed" 干瞪眼。
//
// 🔴 未知值收敛为 **running（非终态）**，不是 succeeded 也不是 failed。
// 当终态落库会提前释放槽位、提前对账，而且客户端再也不会来查。
func NormalizeVideoStatus(raw string) (status string, known bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "running", false
	}
	switch {
	case strings.Contains(s, "cancel"):
		return "cancelled", true
	case strings.Contains(s, "expire"), strings.Contains(s, "timeout"), strings.Contains(s, "time_out"):
		return "expired", true
	case strings.Contains(s, "fail"), strings.Contains(s, "error"):
		return "failed", true
	case strings.Contains(s, "success"), strings.Contains(s, "succeed"),
		strings.Contains(s, "completed"), strings.Contains(s, "complete"),
		strings.Contains(s, "done"), strings.Contains(s, "finish"):
		return "succeeded", true
	case strings.Contains(s, "queue"), strings.Contains(s, "pending"),
		strings.Contains(s, "wait"), strings.Contains(s, "init"), strings.Contains(s, "created"):
		return "queued", true
	case strings.Contains(s, "run"), strings.Contains(s, "process"),
		strings.Contains(s, "generat"), strings.Contains(s, "progress"):
		return "running", true
	}
	return "running", false
}

// videoProductFields 产物字段的候选名（上游拼写不稳定，按顺序找第一个 http 值）。
var videoProductFields = []string{
	"video_url", "videoUrl", "url", "output_url", "outputUrl",
}

// VideoProduct 从任务对象里抽产物。
//
// 返回 (videoURL, lastFrameURL)。**只有 succeeded 才该有值** ——
// 未成功就交地址等于撒谎（skill §2.3）。
func VideoProduct(raw map[string]any) (video, lastFrame string) {
	if raw == nil {
		return "", ""
	}
	// content 可能是字符串（直链），也可能是对象。
	switch c := raw["content"].(type) {
	case string:
		if isHTTPURL(c) {
			video = c
		}
	case map[string]any:
		video = firstHTTP(c, videoProductFields...)
		lastFrame = firstHTTP(c, "last_frame_url", "lastFrameUrl", "tail_image_url")
	}

	if video == "" {
		video = firstHTTP(raw, videoProductFields...)
	}
	if lastFrame == "" {
		lastFrame = firstHTTP(raw, "last_frame_url", "lastFrameUrl", "tail_image_url")
	}

	// 数组形态：contents / videos / outputs，取第一个带 url 的条目。
	if video == "" {
		for _, key := range []string{"contents", "videos", "outputs", "results"} {
			arr, ok := raw[key].([]any)
			if !ok {
				continue
			}
			for _, item := range arr {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if u := firstHTTP(m, videoProductFields...); u != "" {
					video = u
					break
				}
			}
			if video != "" {
				break
			}
		}
	}
	return video, lastFrame
}

// VideoFailure 抽上游失败信息。
//
// error 可能是字符串，也可能是 {code, message} 对象；code 与 message 分开放，
// 让归因层（server 侧）决定翻成 content_filter 还是 upstream_error。
func VideoFailure(raw map[string]any) (code, message string) {
	if raw == nil {
		return "", ""
	}
	if code == "" {
		code = firstString(raw, "error_code", "errorCode", "code", "fail_code", "task_status_reason")
	}
	if message == "" {
		message = firstString(raw, "error_message", "errorMessage", "message", "fail_reason", "reason")
	}
	switch e := raw["error"].(type) {
	case string:
		if message == "" {
			message = e
		}
		if code == "" && !isHTTPURL(e) && strings.TrimSpace(e) != "" {
			// 上游把 code 塞进 error 字符串的形态：短、无空格、非 URL ⇒ 当 code。
			if len(e) <= 64 && !strings.Contains(e, " ") {
				code = e
			}
		}
	case map[string]any:
		if c := firstString(e, "code", "error_code", "type"); c != "" && code == "" {
			code = c
		}
		if m := firstString(e, "message", "msg", "detail"); m != "" && message == "" {
			message = m
		}
	}
	if code == "" && message == "" {
		// 两者都抽不到时，把状态原文当 code —— 至少保留可归因的线索。
		code = firstString(raw, "task_status", "status")
	}
	return code, message
}

// VideoUsage 抽用量。视频模型不统计输入 token，所以 total == completion。
//
// ok=false 表示上游根本没给 —— 此时**不下发 usage 字段**，
// 而不是填一对 0（0 是「确实没用量」，不是「不知道」）。
func VideoUsage(raw map[string]any) (completion, total int, ok bool) {
	if raw == nil {
		return 0, 0, false
	}
	src := raw
	if u, isMap := raw["usage"].(map[string]any); isMap {
		src = u
	}
	completion = firstInt(src, "completion_tokens", "completionTokens", "output_tokens")
	total = firstInt(src, "total_tokens", "totalTokens")
	if completion == 0 && total == 0 {
		return 0, 0, false
	}
	if total == 0 {
		total = completion
	}
	return completion, total, true
}

// VideoStatusRaw 取上游状态原文（task_status 优先，其次 status）。
func VideoStatusRaw(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	if s := firstString(raw, "task_status", "taskStatus", "status"); s != "" {
		return s
	}
	return ""
}

// VideoExtras 视频 biz_data 的扩展位。
//
// 取证来源：上游前端 bundle 0.3.38 的 genVideo 构造器（2026-11-08 只读）：
//
//	genMode: mode === "multi-reference" ? "multi_ref" : "vid_gen"
//	params:  {resolution, size, duration, audio:true,
//	          ...(attachments.length ? {attachments:[{type:"image",materialId}]} : {})}
//
// 🔴 两条硬事实：
//   - 参考图在 `params.attachments`，**不是** `files[]` 里的图片会自动被用；
//   - `attachments` 的顺序就是语义：首尾帧场景 [0]=首帧、[1]=尾帧。
type VideoExtras struct {
	// GenMode biz_data.req.genMode：vid_gen（文生 / 首尾帧）或 multi_ref（多参考图）。
	GenMode string
	// VideoMode extra.meta.videoGenerationMode：standard / multi-reference / first-last-frame。
	// 三值，与 GenMode 的两值**不是同一套**，两者都要下发。
	VideoMode string
	// Attachments 素材 ID 列表（materialId），顺序即语义。
	Attachments []string
}

// VideoSpecEx 构造 t2v 提交规格。
//
// biz_data 是**字符串化的 JSON**（不是对象）—— 写成对象上游直接 Bad_Request。
// 用 map 构造再 Marshal，而不是手拼 fmt.Sprintf：手拼少一个引号就是 400，
// 而且加字段时容易漏逗号。
func VideoSpecEx(size, resolution string, duration int, audio bool, rootModel string,
	extras VideoExtras) ChatSpec {

	if extras.VideoMode == "" {
		extras.VideoMode = "standard"
	}

	if resolution == "" {
		resolution = "720P"
	}
	if duration <= 0 {
		duration = 5
	}
	if rootModel == "" {
		rootModel = "happyhorse11"
	}
	genMode := extras.GenMode
	if genMode == "" {
		genMode = "vid_gen"
	}
	params := map[string]any{
		"resolution": resolution,
		"size":       size,
		"duration":   duration,
		"audio":      audio,
	}
	if len(extras.Attachments) > 0 {
		arr := make([]map[string]any, 0, len(extras.Attachments))
		for _, id := range extras.Attachments {
			arr = append(arr, map[string]any{"type": "image", "materialId": id})
		}
		params["attachments"] = arr
	}
	biz, err := json.Marshal(map[string]any{
		"bizScene": "genVideo",
		"req": map[string]any{
			"rootModel": rootModel,
			"genMode":   genMode,
			"params":    params,
		},
	})
	if err != nil {
		// 全是字符串/数字/布尔，Marshal 不可能失败；失败就退回最小可用形态。
		biz = []byte(fmt.Sprintf(
			`{"bizScene":"genVideo","req":{"rootModel":%q,"genMode":%q,"params":{"resolution":%q,"size":%q,"duration":%d,"audio":%t}}}`,
			rootModel, genMode, resolution, size, duration, audio))
	}
	return ChatSpec{
		ChatType:    "t2v",
		SubChatType: "zaodian_generate_video",
		Size:        size,
		BizData:     string(biz),
		VideoMode:   extras.VideoMode,
	}
}

// ---------- 小工具 ----------

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstHTTP(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && isHTTPURL(v) {
			return v
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		}
	}
	return 0
}
