package server

// canonical 异步视频协议（video-adapter §2）的纯函数层。
//
// 这一层只做三件事，全都不碰网络：
//   - 把客户端 JSON 解成 canonical 请求（含未知键透传）；
//   - 按固定顺序校验（content → model → 场景互斥 → 取值域）；
//   - 把「参数双载体」（body 字段 / prompt 里的 `--rs` 后缀）折叠成一份执行计划。
//
// 🔴 校验顺序不可颠倒。四类拒绝的优先级决定了错误信息指向谁：
// 先判 content 再判 model —— content 为空时谈 model 没有意义；
// 场景互斥必须早于取值域 —— 否则「首尾帧混了参考图」会被报成「ratio 不合法」，
// 调用方照着改 ratio，越改越远。
//
// 🔴 归一化与拒绝的分界线在「会不会改变账单」：
// 「没说」（字段缺省）可以归一化；「说了个错的」不行。
// resolution / duration / frames / ratio 属于计费相关，错值一律 400，绝不回落默认。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// 六态。非终态：queued / running；终态：succeeded / failed / expired / cancelled。
const (
	VideoStatusQueued    = "queued"
	VideoStatusRunning   = "running"
	VideoStatusSucceeded = "succeeded"
	VideoStatusFailed    = "failed"
	VideoStatusExpired   = "expired"
	VideoStatusCancelled = "cancelled"
)

// IsTerminalVideoStatus 终态判定。
//
// 未知值**不是**终态 —— 上游新增一个 done_pending 之类的中间态时，
// 把它当终态会提前释放槽位、提前落库，且客户端再也不会来查。
func IsTerminalVideoStatus(s string) bool {
	switch s {
	case VideoStatusSucceeded, VideoStatusFailed, VideoStatusExpired, VideoStatusCancelled:
		return true
	}
	return false
}

// VideoAssetRef 素材引用（`{"url": "..."}`）。
type VideoAssetRef struct {
	URL string `json:"url"`
}

// VideoDraftRef 样片任务引用（`{"id": "..."}`）。
type VideoDraftRef struct {
	ID string `json:"id"`
}

// VideoContent content[] 的一项。
//
// 每个元素同时表达**载体**（type）与**语义位置**（role）：
// 载体决定怎么取字节，role 决定上游把它放在首帧 / 尾帧 / 参考位。
// 少了 role，多图请求会被上游按自己的默认解释 —— 「首尾帧变参考图」就是这么来的。
type VideoContent struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ImageURL  *VideoAssetRef `json:"image_url,omitempty"`
	VideoURL  *VideoAssetRef `json:"video_url,omitempty"`
	AudioURL  *VideoAssetRef `json:"audio_url,omitempty"`
	DraftTask *VideoDraftRef `json:"draft_task,omitempty"`
	Role      string         `json:"role,omitempty"`
}

// VideoRequest canonical 创建请求。
//
// 指针字段的语义是「调用方有没有说」：nil = 没下发（用上游默认），
// 非 nil = 明确要求。默认值不主动下发是协议纪律 —— 网关替调用方
// 填一个默认值，等于把上游默认值改成了网关的猜测。
type VideoRequest struct {
	Model   string         `json:"model"`
	Content []VideoContent `json:"content"`

	// Duration 秒；-1 = 模型自选（上游不支持，会被拒）。
	Duration *int `json:"duration,omitempty"`
	// Frames 与 duration 二选一，frames 优先（上游不支持，会被拒）。
	Frames *int `json:"frames,omitempty"`

	Resolution string `json:"resolution,omitempty"`
	Ratio      string `json:"ratio,omitempty"`

	GenerateAudio   *bool `json:"generate_audio,omitempty"`
	Watermark       *bool `json:"watermark,omitempty"`
	CameraFixed     *bool `json:"camera_fixed,omitempty"`
	ReturnLastFrame *bool `json:"return_last_frame,omitempty"`
	Seed            *int  `json:"seed,omitempty"`

	OutputFormat  string `json:"output_format,omitempty"`
	Draft         *bool  `json:"draft,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
	CallbackURL   string `json:"callback_url,omitempty"`
	Priority      *int   `json:"priority,omitempty"`
	SafetyID      string `json:"safety_identifier,omitempty"`
	OmniTaskType  string `json:"omni_reference_task_type,omitempty"`
	ExpiresAfter  *int   `json:"execution_expires_after,omitempty"`
	Tools         []map[string]any `json:"tools,omitempty"`

	// Extras 未识别键原样保留（vendor extras 透传，不收窄 additionalProperties）。
	Extras map[string]any `json:"-"`
}

// videoKnownKeys 已识别的顶层键 —— 其余全部进 Extras 透传。
var videoKnownKeys = map[string]struct{}{
	"model": {}, "content": {}, "duration": {}, "frames": {},
	"resolution": {}, "ratio": {}, "generate_audio": {}, "watermark": {},
	"camera_fixed": {}, "return_last_frame": {}, "seed": {},
	"output_format": {}, "draft": {}, "service_tier": {}, "callback_url": {},
	"priority": {}, "safety_identifier": {}, "omni_reference_task_type": {},
	"execution_expires_after": {}, "tools": {},
}

// videoMaxBody canonical 请求体上限。参考素材走 URL，不进 body；
// 给 64MB 是留给「图片走 data URI」的内联场景（与图像协议同口径）。
const videoMaxBody = 64 << 20

// 方舟错误码（`error.code` 的取值）。
//
// 🔴 官方错误码页明确「未提供通用 HTTP 错误响应体的 JSON 形状」，
// 所以四键信封是本协议自己的契约，code 用方舟的**码字符串**只为让方舟客户端能按类别分支 ——
// 不是「照官方回包」。三处必须分清：
//   type   —— 哪一类错（OpenAI 词表，客户端按它决定重试策略）
//   code   —— 具体是什么（方舟词表，客户端按它决定要不要改参数）
//   param  —— 指到真实字段（指错比不指更糟）
const (
	arkMissing      = "MissingParameter"
	arkInvalid      = "InvalidParameter"
	arkUnsupported  = "InvalidParameter.UnsupportedParameter"
	arkTaskType     = "InvalidParameter.TaskTypeConstraint"
	arkDenied       = "InvalidParameter.OperationDenied"
	arkNotFound     = "ResourceNotFound"
	arkContentFilte = "InputVideoSensitiveContentDetected"
)

// videoReject 协议级拒绝（转换点 ⑥ 的提交期分支）。
//
// 四键信封只在这一层产生：按「谁的错」翻，不照抄上游的码。
// param 只在能定位到客户端真实字段时才给 —— 给错比不给更糟。
type videoReject struct {
	Status   int
	Type     string
	Code     string
	Param    string
	Message  string
	Warnings []string
}

func (e *videoReject) Error() string { return e.Message }

func rejectInvalid(param, msg string) *videoReject {
	return &videoReject{Status: 400, Type: "invalid_request_error", Code: arkInvalid, Param: param, Message: msg}
}

// rejectMissing 缺参数（方舟把「没给」与「给错」分成两个码）。
func rejectMissing(param, msg string) *videoReject {
	return &videoReject{Status: 400, Type: "invalid_request_error", Code: arkMissing, Param: param, Message: msg}
}

func rejectUnsupported(param, msg string) *videoReject {
	return &videoReject{Status: 400, Type: "invalid_request_error", Code: arkUnsupported, Param: param, Message: msg}
}

// rejectTaskType 三类互斥场景混用 —— 方舟有专门的码。
func rejectTaskType(param, msg string) *videoReject {
	return &videoReject{Status: 400, Type: "invalid_request_error", Code: arkTaskType, Param: param, Message: msg}
}

// decodeVideoRequest 解 JSON 体并收集未知键。
//
// 两遍解码（先 map 再 struct）而不是自定义 UnmarshalJSON：
// 未知键透传是协议要求，而 struct 解码天然丢键 —— 先留一份原始 map 最省事。
func decodeVideoRequest(r io.Reader) (*VideoRequest, *videoReject) {
	raw, err := io.ReadAll(io.LimitReader(r, videoMaxBody))
	if err != nil {
		return nil, rejectInvalid("", "读取请求体失败: "+err.Error())
	}
	if len(raw) == 0 {
		return nil, rejectInvalid("", "请求体为空")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		// JSON 体不是对象（数组 / 标量 / 语法错）—— 无从解释，直接拒。
		return nil, rejectInvalid("", "请求体必须是 JSON 对象: "+err.Error())
	}

	var req VideoRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, rejectInvalid("", "请求体解析失败: "+err.Error())
	}

	for k, v := range top {
		if _, known := videoKnownKeys[k]; known {
			continue
		}
		if req.Extras == nil {
			req.Extras = map[string]any{}
		}
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			req.Extras[k] = val
		}
	}
	return &req, nil
}

// videoClip 折叠后的一项参考素材（准备上传 / 直通的那一份）。
type videoClip struct {
	Kind string // image / video / audio
	Role string // first_frame / last_frame / reference_image / reference_video / reference_audio
	Ref  string // URL / data URI / 裸 base64 / asset://
}

// videoPlan 折叠后的执行计划：上游真正需要的那几个值。
type videoPlan struct {
	Prompt  string  // 已剥离参数后缀的正文
	Scene   string  // t2v / first_frame / first_last / omni_ref
	Clips   []videoClip

	Resolution string // 上游拼写：720P / 1080P
	Ratio      string // 上游拼写：16:9 / 4:3 / 1:1 / 3:4 / 9:16 / origin
	Duration   int
	Audio      bool

	// RootModel 生成内核（happyhorse11 / happyhorse / wan27）。
	// 能力按内核分档，所以校验阶段就要解析出来，不能等到提交。
	RootModel string
	// GenMode 上游 biz_data.req.genMode：vid_gen（文生/首尾帧）或 multi_ref（多参考）。
	GenMode string
	// VideoMode 上游 extra.meta.videoGenerationMode：standard / multi-reference / first-last-frame。
	//
	// 🔴 同一个「模式」在上游有**两个载体、两套取值**：
	//   extra.meta.videoGenerationMode ∈ {standard, multi-reference, first-last-frame}
	//   biz_data.req.genMode             ∈ {vid_gen, multi_ref}
	// 首尾帧与普通文生的 genMode **都是 vid_gen**，唯一的区分就是 videoGenerationMode。
	// 只写 genMode 不写它 ⇒ 上游按纯文生处理：attachments[0] 当参考图，第二张直接忽略。
	// （2026-11-08 实测踩到：红色首帧生效、蓝色尾帧被丢，产物 5 秒全红。）
	VideoMode string

	// 回显用（客户端原名原值，不许被上游 ID 覆盖）。
	EchoModel      string
	EchoResolution string
	EchoRatio      string
	EchoDuration   int
	EchoAudio      bool

	CallbackURL  string
	ExpiresAfter int
	Warnings     []string
	Extras       map[string]any
	// Fingerprint 幂等键：同一份意图重复提交必须命中同一个任务。
	Fingerprint string
}

// 上游能力矩阵（按**内核**分档）。
//
// 取证来源：上游前端 bundle `qwen-chat-fe/0.3.38/js/main.js`（2026-11-08 只读取件，零消耗）。
// 原文常量：
//
//	ss = {"happyhorse-1.1":["standard","multi-reference"],
//	      "happyhorse-1.0":["standard","multi-reference"],
//	      "wan-2.7":["standard","first-last-frame"]}
//	ms = {standard:1, "multi-reference":9, "first-last-frame":2}   // 各模式附件张数
//
// 即：happyhorse 系只能做「文生 + 多参考图（≤9 张）」，
// wan-2.7 只能做「文生 + 首尾帧（2 张）」，**没有内核同时支持两种**。
//
// 🔴 这是「枚举是矩阵」的典型：同一个 content[] 组合，换内核就可能从合法变非法。
// 所以能力闸门必须在模型解析之后（plan.RootModel），不能只看请求形状。
type videoKernelCaps struct {
	// Standard 纯文生视频。
	Standard bool
	// MultiReference 多参考图上限（0 = 不支持）。
	MultiReference int
	// FirstLastFrame 首帧 / 首尾帧。
	FirstLastFrame bool
}

var videoKernelCapsTable = map[string]videoKernelCaps{
	"happyhorse11": {Standard: true, MultiReference: 9},
	"happyhorse":   {Standard: true, MultiReference: 9},
	"wan27":        {Standard: true, FirstLastFrame: true},
}

// videoKernelAliases 内核别名 → 规范名（key 是 modelKey 归一化后的形态）。
var videoKernelAliases = map[string]string{
	"happyhorse11": "happyhorse11",
	"happyhorse":   "happyhorse",
	"happyhorse10": "happyhorse", // happyhorse-1.0
	"wan27":        "wan27",      // wan-2.7
}

// videoMaterialEnabled 参考素材能力（默认开，可用环境变量紧急关闭）。
//
// ✅ 2026-11-08 实测通过（wan27 + 首帧纯红/尾帧纯蓝，720p/5s）：
//   - 提交 200 + task id，47s 出片，产物路径 `.../i2v/...`（文生是 `t2v`）
//   - 抽帧：首帧 R=252/B=0、末帧 R=0/B=244、第 75 帧 R=235/B=11（正在过渡）
//   ⇒ `files[]` 上传后拿到的 `id`（`img-<uuid>`）**可以直接当 materialId**。
//
// 保留开关是为了「上游哪天改了素材体系」时能一键退回纯文生，不必回滚版本：
// `QWEN2API_VIDEO_MATERIAL_DISABLE=1`。
var videoMaterialEnabled = os.Getenv("QWEN2API_VIDEO_MATERIAL_DISABLE") != "1"

// 上游支持的取值域（前端 bundle 实证）。
var (
	upstreamVideoResolutions = []string{"720P", "1080P"}
	// Jn 常量：16:9 / 4:3 / 1:1 / 3:4 / 9:16 / original。
	// 比我们此前按 UI 猜的 3 个多一倍 —— 「未测 ≠ 不支持」的活例子。
	upstreamVideoRatios    = []string{"16:9", "4:3", "1:1", "3:4", "9:16"}
	upstreamVideoDurations = []int{5, 10, 15}
)

// validateVideoRequest 按固定顺序校验并折叠成执行计划。
func (s *Server) validateVideoRequest(req *VideoRequest) (*videoPlan, *videoReject) {
	plan := &videoPlan{Extras: req.Extras}

	// ---- ① content ----
	if len(req.Content) == 0 {
		return nil, rejectMissing("content", `'content' 必须是非空数组，至少一项（纯文生视频也要一项 text）`)
	}
	texts := make([]string, 0, 2)
	images := make([]videoClip, 0, 4)
	videos := make([]videoClip, 0, 2)
	audios := make([]videoClip, 0, 2)

	for i, item := range req.Content {
		switch item.Type {
		case "text":
			if t := strings.TrimSpace(item.Text); t != "" {
				texts = append(texts, t)
			}
			// 空串 text 是「这项不重要」的安全表达 ⇒ 删项，不拒绝。
		case "image_url":
			if item.ImageURL == nil || strings.TrimSpace(item.ImageURL.URL) == "" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].image_url", i), "图片引用的 url 不能为空")
			}
			role, rj := normalizeImageRole(item.Role, i)
			if rj != nil {
				return nil, rj
			}
			images = append(images, videoClip{Kind: "image", Role: role, Ref: strings.TrimSpace(item.ImageURL.URL)})
		case "video_url":
			if item.VideoURL == nil || strings.TrimSpace(item.VideoURL.URL) == "" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].video_url", i), "视频引用的 url 不能为空")
			}
			if item.Role != "reference_video" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].role", i),
					`video_url 的 role 必须是 "reference_video"`);
			}
			ref := strings.TrimSpace(item.VideoURL.URL)
			if strings.HasPrefix(strings.ToLower(ref), "data:") {
				return nil, rejectUnsupported(fmt.Sprintf("content[%d].video_url", i),
					"视频素材不支持 base64/data URI：只收公网 URL 或 asset://（上游只给 URL）")
			}
			videos = append(videos, videoClip{Kind: "video", Role: item.Role, Ref: ref})
		case "audio_url":
			if item.AudioURL == nil || strings.TrimSpace(item.AudioURL.URL) == "" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].audio_url", i), "音频引用的 url 不能为空")
			}
			if item.Role != "reference_audio" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].role", i),
					`audio_url 的 role 必须是 "reference_audio"`);
			}
			audios = append(audios, videoClip{Kind: "audio", Role: item.Role, Ref: strings.TrimSpace(item.AudioURL.URL)})
		case "draft_task":
			if item.DraftTask == nil || strings.TrimSpace(item.DraftTask.ID) == "" {
				return nil, rejectInvalid(fmt.Sprintf("content[%d].draft_task", i), "draft_task.id 不能为空")
			}
			return nil, rejectUnsupported(fmt.Sprintf("content[%d]", i),
				"本上游没有「样片 / draft」概念，draft_task 无法映射")
		case "":
			return nil, rejectInvalid(fmt.Sprintf("content[%d].type", i), "content 项缺少 type")
		default:
			return nil, rejectInvalid(fmt.Sprintf("content[%d].type", i),
				fmt.Sprintf("未知的 content type %q（支持 text / image_url / video_url / audio_url）", item.Type))
		}
	}

	// ---- ② model ----
	plan.EchoModel = strings.TrimSpace(req.Model)
	if plan.EchoModel == "" {
		return nil, rejectMissing("model", "缺少 model（路由名，可以是视频内核名，也可以是任意 chat 模型名）")
	}

	// ---- ③ 场景互斥 ----
	scene, rj := judgeVideoScene(images, videos, audios)
	if rj != nil {
		return nil, rj
	}
	plan.Scene = scene
	// 能力按内核分档 —— 必须在场景判定之后、能力闸门之前解析。
	_, plan.RootModel = s.resolveVideoModel(plan.EchoModel)
	plan.GenMode = "vid_gen"
	plan.VideoMode = "standard"
	switch scene {
	case "omni_ref":
		plan.GenMode = "multi_ref"
		plan.VideoMode = "multi-reference"
	case "first_last":
		// genMode 仍是 vid_gen；区别只在 videoGenerationMode。
		plan.VideoMode = "first-last-frame"
	case "first_frame":
		// 单图首帧走 standard：前端 ms[standard]=1，一张附件就是它。
		plan.VideoMode = "standard"
	}
	if rj := checkVideoCaps(plan, images, videos, audios); rj != nil {
		return nil, rj
	}

	// ---- ④ 取值域 ----
	prompt, sfx, sfxWarn, rj := foldPromptSuffix(strings.Join(texts, "\n"), req)
	if rj != nil {
		return nil, rj
	}
	plan.Prompt = prompt

	resolution := strings.TrimSpace(req.Resolution)
	if sfx.Resolution != "" {
		resolution = sfx.Resolution
	}
	plan.EchoResolution, plan.Resolution, rj = foldResolution(resolution)
	if rj != nil {
		return nil, rj
	}

	ratio := strings.TrimSpace(req.Ratio)
	if sfx.Ratio != "" {
		ratio = sfx.Ratio
	}
	plan.EchoRatio, plan.Ratio, rj = foldRatio(ratio)
	if rj != nil {
		return nil, rj
	}

	dur, rj := foldDuration(req.Duration, sfx.Duration)
	if rj != nil {
		return nil, rj
	}
	plan.Duration, plan.EchoDuration = dur, dur

	audio := req.GenerateAudio
	if sfx.Audio != nil {
		audio = sfx.Audio
	}
	plan.Audio = audio != nil && *audio
	plan.EchoAudio = plan.Audio

	if req.Frames != nil {
		return nil, rejectUnsupported("frames",
			"本上游只认秒级 duration（5/10/15），不支持 frames；请改用 duration")
	}
	if plan.Prompt == "" && len(images) == 0 && len(videos) == 0 && len(audios) == 0 {
		return nil, rejectInvalid("content", "content 里没有任何可用内容（text 全为空且无参考素材）")
	}

	plan.CallbackURL = strings.TrimSpace(req.CallbackURL)
	if plan.CallbackURL != "" && !strings.HasPrefix(plan.CallbackURL, "http://") &&
		!strings.HasPrefix(plan.CallbackURL, "https://") {
		return nil, rejectInvalid("callback_url", "callback_url 必须是 http(s) 绝对地址")
	}

	// 超时阈值：canonical 默认 48h、范围 [1h, 72h]。网关本地判定用。
	plan.ExpiresAfter = 48 * 3600
	if req.ExpiresAfter != nil {
		if *req.ExpiresAfter < 3600 || *req.ExpiresAfter > 259200 {
			return nil, rejectInvalid("execution_expires_after",
				"execution_expires_after 取值范围是 3600~259200 秒（1h~72h）")
		}
		plan.ExpiresAfter = *req.ExpiresAfter
	}

	// 上游没有对应参数的字段：不静默丢，也不拦死 —— 记进 warnings，如实告知。
	plan.Warnings = append(plan.Warnings, sfxWarn...)
	plan.Warnings = append(plan.Warnings, unsupportedExtras(req)...)

	plan.Clips = append(plan.Clips, images...)
	plan.Clips = append(plan.Clips, videos...)
	plan.Clips = append(plan.Clips, audios...)

	plan.Fingerprint = videoFingerprint(plan)
	return plan, nil
}

// normalizeImageRole 校正图片 role。
//
// 省略 role 只在「单图首帧」场景成立；这里先归一成显式值，
// 到场景判定时再按数量判互斥。
func normalizeImageRole(role string, idx int) (string, *videoReject) {
	switch role {
	case "":
		return "first_frame", nil // 单图省略 ⇒ 首帧；多图时由场景判定拒绝
	case "first_frame", "last_frame", "reference_image":
		return role, nil
	default:
		return "", rejectInvalid(fmt.Sprintf("content[%d].role", idx),
			fmt.Sprintf(`图片 role 只能是 first_frame / last_frame / reference_image，得到 %q`, role))
	}
}

// judgeVideoScene 三类互斥场景：首帧 / 首尾帧 / 全模态参考。
//
// 混用即 400 —— 首帧、首尾帧、全模态参考是三种不同任务，
// 混着发上游按自己的默认解释，产物与调用方预期不同，而且已经计费。
func judgeVideoScene(images, videos, audios []videoClip) (string, *videoReject) {
	hasRefVideo := len(videos) > 0 || len(audios) > 0

	firsts, lasts, refs := 0, 0, 0
	for _, im := range images {
		switch im.Role {
		case "first_frame":
			firsts++
		case "last_frame":
			lasts++
		case "reference_image":
			refs++
		}
	}

	if firsts+lasts > 0 && refs > 0 {
		return "", rejectTaskType("content",
			"首帧/尾帧（first_frame/last_frame）与参考图（reference_image）不能混用：它们是两种不同任务")
	}
	if hasRefVideo && (firsts+lasts > 0) {
		return "", rejectTaskType("content",
			"参考视频/音频只能与 reference_image 组合（全模态参考场景），不能与首帧/尾帧混用")
	}

	switch {
	case len(images) == 0 && !hasRefVideo:
		return "t2v", nil
	case len(images) == 0 && hasRefVideo:
		// 只有视频/音频参考：场景上属于「全模态参考」，由能力闸门给出
		// 「本上游没有参考视频/音频能力」这句准确的拒绝，而不是在这里
		// 报一句含混的「图片组合不合法」。
		return "omni_ref", nil
	case len(images) == 1 && firsts == 1 && !hasRefVideo:
		return "first_frame", nil
	case len(images) == 2 && firsts == 1 && lasts == 1 && !hasRefVideo:
		return "first_last", nil
	case lasts > 0 && firsts == 0:
		return "", rejectTaskType("content",
			"只给了尾帧（last_frame）没有首帧：首尾帧场景要求两张图都显式给 role")
	case refs == len(images) && refs > 0:
		return "omni_ref", nil
	}
	return "", rejectTaskType("content",
		fmt.Sprintf("图片组合不合法（首帧 %d / 尾帧 %d / 参考图 %d）："+
			"首帧场景 1 张、首尾帧场景 2 张且 role 显式、参考图场景全部标 reference_image",
			firsts, lasts, refs))
}

// checkVideoCaps 能力闸门：场景/素材超出上游**已取证**能力 ⇒ 400 unsupported_parameter。
//
// 🔴 「未取证 ≠ 不支持」，但也不能假装支持。所以拒绝时把边界写进 message：
// 让调用方知道这是上游能力边界，不是自己写错了字段 ——
// 这两种情况的处置完全不同（改请求 vs 换上游）。
func checkVideoCaps(plan *videoPlan, images, videos, audios []videoClip) *videoReject {
	// 参考视频 / 音频：前端 bundle 的 attachments 只有 type:"image"，
	// 没有任何视频/音频素材的构造路径 ⇒ 明确不支持（不是「未取证」）。
	if len(videos) > 0 || len(audios) > 0 {
		return rejectUnsupported("content",
			"本上游没有参考视频 / 参考音频能力：attachments 只接受 {type:\"image\"}")
	}

	caps, known := videoKernelCapsTable[plan.RootModel]
	if !known {
		caps = videoKernelCaps{Standard: true}
	}

	switch plan.Scene {
	case "t2v":
		if !caps.Standard {
			return rejectUnsupported("model", "内核 "+plan.RootModel+" 不支持任何视频生成模式")
		}
		return nil
	case "first_frame", "first_last":
		if !caps.FirstLastFrame {
			return rejectUnsupported("content", fmt.Sprintf(
				"内核 %s 不支持首帧/首尾帧（只有 wan27 支持）；"+
					"要做参考图请把 role 改成 reference_image", plan.RootModel))
		}
		if !videoMaterialEnabled {
			return rejectUnsupported("content",
				"参考素材能力已通过 QWEN2API_VIDEO_MATERIAL_DISABLE=1 关闭："+
					"当前只接受纯文本 content（t2v）")
		}
		return nil
	case "omni_ref":
		if caps.MultiReference == 0 {
			return rejectUnsupported("content", fmt.Sprintf(
				"内核 %s 不支持参考图（只有 happyhorse 系支持，最多 9 张）", plan.RootModel))
		}
		if len(images) > caps.MultiReference {
			return rejectUnsupported("content", fmt.Sprintf(
				"内核 %s 最多 %d 张参考图，给了 %d 张",
				plan.RootModel, caps.MultiReference, len(images)))
		}
		if !videoMaterialEnabled {
			return rejectUnsupported("content",
				"参考素材能力已通过 QWEN2API_VIDEO_MATERIAL_DISABLE=1 关闭："+
					"当前只接受纯文本 content（t2v）")
		}
		return nil
	}
	return nil
}

// 参数后缀（弱校验载体的）折叠结果。
type videoSuffix struct {
	Resolution string
	Ratio      string
	Duration   *int
	Audio      *bool
	Warnings   []string
}

// foldPromptSuffix 解析并**剥离** prompt 末尾的 `--rs / --rt / --dur / --wm` 后缀。
//
// 🔴 两件事必须同时做：回填字段 + 从文本里删掉。
// 只回填不剥离 ⇒ 后缀会被模型当成提示词内容读进画面（可能出现在字幕里）；
// 只剥离不回填 ⇒ 参数根本没下发，客户端看到 200、任务成功，只是时长比例不对。
//
// 强校验（body 字段）与弱校验（后缀）**绝不同时下发**：优先级不明，且会污染语义。
// 所以两边都给了就是 400，让调用方自己选一种。
func foldPromptSuffix(prompt string, req *VideoRequest) (string, videoSuffix, []string, *videoReject) {
	var sfx videoSuffix
	clean, pairs := splitTrailingFlags(prompt)
	if len(pairs) == 0 {
		return prompt, sfx, nil, nil
	}

	for _, p := range pairs {
		switch p.key {
		case "rs", "resolution":
			if req.Resolution != "" {
				return "", sfx, nil, rejectInvalid("resolution",
					"resolution 同时出现在 body 与 prompt 后缀里：参数双载体不能同时下发，请只保留一种")
			}
			sfx.Resolution = p.val
		case "rt", "ratio":
			if req.Ratio != "" {
				return "", sfx, nil, rejectInvalid("ratio",
					"ratio 同时出现在 body 与 prompt 后缀里：参数双载体不能同时下发，请只保留一种")
			}
			sfx.Ratio = p.val
		case "dur", "duration":
			if req.Duration != nil {
				return "", sfx, nil, rejectInvalid("duration",
					"duration 同时出现在 body 与 prompt 后缀里：参数双载体不能同时下发，请只保留一种")
			}
			n, err := strconv.Atoi(p.val)
			if err != nil {
				return "", sfx, nil, rejectInvalid("duration",
					"prompt 后缀 --dur 的值不是整数: "+p.val)
			}
			sfx.Duration = &n
		case "wm", "watermark":
			if _, ok := parseFlagBool(p.val); !ok {
				return "", sfx, nil, rejectInvalid("watermark", "prompt 后缀 --wm 的值不是布尔: "+p.val)
			}
			sfx.Warnings = append(sfx.Warnings,
				"prompt 后缀 --wm 已剥离，但本上游没有水印参数，未下发")
		case "cf", "camerafixed":
			if _, ok := parseFlagBool(p.val); !ok {
				return "", sfx, nil, rejectInvalid("", "prompt 后缀 --cf 的值不是布尔: "+p.val)
			}
			sfx.Warnings = append(sfx.Warnings,
				"prompt 后缀 --cf 已剥离，但本上游没有固定机位参数，未下发")
		case "seed":
			sfx.Warnings = append(sfx.Warnings,
				"prompt 后缀 --seed 已剥离，但本上游不返回可复现的 seed，未下发")
		default:
			// 无法识别的后缀：**不吞**。留在文本里会被模型读进画面，
			// 删了又等于替调用方决定 —— 记 warning，正文保持原样（连同后缀）。
			sfx.Warnings = append(sfx.Warnings,
				fmt.Sprintf("prompt 里的 --%s 不是已知参数后缀，已保留在正文中", p.key))
		}
	}
	return clean, sfx, sfx.Warnings, nil
}

type flagPair struct{ key, val string }

// splitTrailingFlags 从末尾向前吃掉连续的 `--key value` / `--key=value` 片段。
//
// 只吃**末尾连续段**：正文中间出现的 `--` 很可能是用户想写进画面的文字，
// 不能反向扫描。
func splitTrailingFlags(s string) (string, []flagPair) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s, nil
	}
	var pairs []flagPair
	// 从右往左吃：`--k v` 是两段、`--k=v` 是一段、裸 `--k` 是布尔真。
	// i 停在「保留的最后一个下标 + 1」，最终保留 fields[:i+1]。
	i := len(fields) - 1
	for i >= 0 {
		f := fields[i]
		if strings.HasPrefix(f, "--") && len(f) > 2 {
			body := f[2:]
			if k, v, ok := strings.Cut(body, "="); ok {
				pairs = append([]flagPair{{key: strings.ToLower(k), val: v}}, pairs...)
				i--
				continue
			}
			pairs = append([]flagPair{{key: strings.ToLower(body), val: "true"}}, pairs...)
			i--
			continue
		}
		// f 是值：只有当它紧跟在**无等号**的 `--flag` 后面才算后缀的值。
		if i > 0 {
			prev := fields[i-1]
			if strings.HasPrefix(prev, "--") && len(prev) > 2 && !strings.Contains(prev, "=") {
				pairs = append([]flagPair{{key: strings.ToLower(prev[2:]), val: f}}, pairs...)
				i -= 2
				continue
			}
		}
		break
	}
	if len(pairs) == 0 {
		return s, nil
	}
	return strings.TrimSpace(strings.Join(fields[:i+1], " ")), pairs
}

func parseFlagBool(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// foldResolution canonical 拼写 → 上游拼写。
//
// 480p / 4k 在上游没有对应档位 ⇒ 400。绝不回落 720P：
// 那会生成另一档分辨率，而账单已经按调用方写的档位产生了预期。
func foldResolution(in string) (echo, upstream string, rj *videoReject) {
	if strings.TrimSpace(in) == "" {
		return "", "", nil // 未说 ⇒ 交给上游默认（由调用处填配置默认）
	}
	switch strings.ToLower(strings.TrimSpace(in)) {
	case "720p":
		return "720p", "720P", nil
	case "1080p":
		return "1080p", "1080P", nil
	}
	return "", "", rejectUnsupported("resolution",
		fmt.Sprintf("本上游不支持 resolution=%q；支持：720p / 1080p", in))
}

// foldRatio 比例：adaptive 映射成上游的 auto，其余严格对照实测域。
func foldRatio(in string) (echo, upstream string, rj *videoReject) {
	if strings.TrimSpace(in) == "" {
		return "", "", nil
	}
	v := strings.TrimSpace(in)
	// adaptive（canonical）与 original（上游 UI 文案）是同一个意思：
	// 由上游按输入内容定比例。发上游时统一写成 "origin"（前端 ns() 的映射）。
	if strings.EqualFold(v, "adaptive") || strings.EqualFold(v, "original") {
		return "adaptive", "origin", nil
	}
	for _, ok := range upstreamVideoRatios {
		if v == ok {
			return v, v, nil
		}
	}
	return "", "", rejectUnsupported("ratio",
		fmt.Sprintf("本上游不支持 ratio=%q；支持：16:9 / 4:3 / 1:1 / 3:4 / 9:16 / adaptive（原图比例）", in))
}

// foldDuration 时长：只认 5 / 10 / 15。-1（模型自选）在上游没有语义，拒。
func foldDuration(in *int, sfx *int) (int, *videoReject) {
	v := -1
	switch {
	case sfx != nil:
		v = *sfx
	case in != nil:
		v = *in
	}
	if v == -1 {
		return 0, rejectUnsupported("duration",
			"本上游不支持 duration=-1（模型自选时长）：时长直接决定积分，必须显式指定 5 / 10 / 15")
	}
	if v <= 0 {
		return 0, nil // 未说 ⇒ 用配置默认
	}
	for _, ok := range upstreamVideoDurations {
		if v == ok {
			return v, nil
		}
	}
	return 0, rejectUnsupported("duration",
		fmt.Sprintf("本上游只支持 duration=5/10/15 秒，得到 %d", v))
}

// unsupportedExtras 上游没有对应字段的可选参数 —— 记 warning，不下发。
func unsupportedExtras(req *VideoRequest) []string {
	var out []string
	add := func(cond bool, field string) {
		if cond {
			out = append(out, field+" 在本上游没有对应参数，未下发")
		}
	}
	add(req.Watermark != nil, "watermark")
	add(req.CameraFixed != nil, "camera_fixed")
	add(req.Seed != nil, "seed")
	add(req.ReturnLastFrame != nil, "return_last_frame")
	add(req.OutputFormat != "", "output_format")
	add(req.Draft != nil, "draft")
	add(req.ServiceTier != "", "service_tier")
	add(req.Priority != nil, "priority")
	add(req.SafetyID != "", "safety_identifier")
	add(req.OmniTaskType != "", "omni_reference_task_type")
	add(len(req.Tools) > 0, "tools")
	return out
}

// videoFingerprint 幂等键。
//
// 🔴 POST 超时后重试 POST = 两个计费任务，而客户端只认得第二个。
// 指纹覆盖「决定这次生成是什么」的全部字段：同一份意图重复提交必须命中同一个任务。
// 不含 callback_url（改回调地址不代表要重新生成）。
func videoFingerprint(p *videoPlan) string {
	var b strings.Builder
	b.WriteString(p.EchoModel)
	b.WriteString("\x00")
	b.WriteString(p.Prompt)
	b.WriteString("\x00")
	b.WriteString(p.Resolution)
	b.WriteString("\x00")
	b.WriteString(p.Ratio)
	b.WriteString("\x00")
	b.WriteString(strconv.Itoa(p.Duration))
	b.WriteString("\x00")
	b.WriteString(strconv.FormatBool(p.Audio))
	for _, c := range p.Clips {
		b.WriteString("\x00")
		b.WriteString(c.Kind)
		b.WriteString(":")
		b.WriteString(c.Role)
		b.WriteString(":")
		b.WriteString(c.Ref)
	}
	return hashString(b.String())
}

// videoTaskID 生成方舟格式的任务 ID：`cgt-<YYYYMMDDhhmmss>-<8 位十六进制>`。
//
// 方舟原生是 `cgt-20261105010835-xxxxx`。客户端不该解析 ID，但格式对齐能让
// 「这个 id 是谁家的」一眼可辨 —— 出问题时少一轮确认。
func videoTaskID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退化成纳秒尾巴：ID 只要求这次进程内不撞。
		sum := sha256.Sum256([]byte(strconv.FormatInt(now.UnixNano(), 10)))
		copy(b[:], sum[:4])
	}
	return "cgt-" + now.Format("20060102150405") + "-" + hex.EncodeToString(b[:])
}

// hashString 指纹用的短哈希（sha256 前 16 字节，够用且日志里不刺眼）。
func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
