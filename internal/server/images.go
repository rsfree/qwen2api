package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- OpenAI Images 前门 ----------
//
// 上游没有"图片端点"：qwen 网页端的图片生成是**模型在 t2t 对话里调 image_gen 工具**
// （2026-10-28 实测：phase=image_gen_tool → function_call.arguments 给出英文 prompt
// → 结束帧 extra.image_list[].image 是带 JWT 的 CDN 直链）。
//
// 所以本端点做的是：把"要一张图"翻译成一次对话，从流里捞图片直链，
// 再按 OpenAI Images 的形态回给调用方。语义上是"代理一个工具调用"，
// 不是"调一个生图 API" —— 这决定了两个诚实边界：
//   - prompt 必须让模型愿意画（本端点会自动补"请画"引导，见 buildImagePrompt）；
//   - **拿不到图就报错**，绝不返回空 data（那会让调用方以为生成成功）。

// ImageRequest `POST /v1/images/generations` 请求体。
//
// 字段集对齐 image-adapter 的规范形态（`docs/04_Spec.md` §5 + `adapter/api/images.py`）：
// 这是 OpenAI images body 的**超集** —— 文生图与图生图走同一个端点，靠 `image` 分流。
//
//	prompt            str（无 image 时必填）
//	image             str | list[str]  URL / data URI / 裸 base64；null 或 [] = 无图
//	mask              str              与 image 同形态（没有 image 时带了就是 400）
//	n                 int              非法值归一为 1（不报错）
//	response_format   "url" | "b64_json"；其它值（含 null/空串）= 未指定，按默认 url
//	size / quality / style / user      原样收下，size 会翻译成上游认识的比例枚举
type ImageRequest struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	// Image 参考图（图生图）。接受字符串或数组 —— 两种 SDK 写法都要收。
	Image json.RawMessage `json:"image,omitempty"`
	Mask  string          `json:"mask,omitempty"`
	// N 生成张数。上游一次工具调用只出一张 ⇒ 多张是多次上游请求，成本线性涨。
	N int `json:"n"`
	// Size 支持 OpenAI 的像素写法（1024x1024）、比例写法（1:1）与 "auto"。
	Size           string `json:"size"`
	Quality        string `json:"quality,omitempty"`
	Style          string `json:"style,omitempty"`
	User           string `json:"user,omitempty"`
	ResponseFormat string `json:"response_format"`
	// Thinking 思考档位（图片场景建议 fast，省时间）。
	Thinking string `json:"thinking"`
}

// imageRefs 解析 image 字段：字符串 / 字符串数组 / null / []。
func (r ImageRequest) imageRefs() ([]string, error) {
	if len(r.Image) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(string(r.Image))
	if raw == "null" {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(r.Image, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			return nil, errors.New("image 不能是空字符串（不传请给 null 或 []）")
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(r.Image, &many); err != nil {
		return nil, errors.New("image 必须是字符串、字符串数组、null 或 []")
	}
	for _, m := range many {
		if strings.TrimSpace(m) == "" {
			return nil, errors.New("image 数组里不能有空字符串")
		}
	}
	return many, nil
}

// maskWithoutImage 判断 mask 是否"非法地单独出现"。
//
// 契约：只有**没有参考图**时带 mask 才是 400；带图时 mask 收下但不转发
// （上游没有 inpaint 语义）。早先的实现无条件 400，把 image+mask 的正常调用
// 也一起打回，错误文案还写着"mask 没有 image"—— 与文档自相矛盾。
func maskWithoutImage(mask string, refs []string) bool {
	return strings.TrimSpace(mask) != "" && len(refs) == 0
}

// ImageDatum 单张结果。
type ImageDatum struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageResponse `POST /v1/images/generations` 响应。
type ImageResponse struct {
	Created int64        `json:"created"`
	Data    []ImageDatum `json:"data"`
}

// sizeToRatio 把 OpenAI 的像素尺寸映射成上游认识的画幅比例。
//
// 上游只吃比例（1:1 / 3:4 / 4:3 / 16:9 / 9:16）；像素值对它是无意义的，
// 所以这里做**有损映射**并在日志里记一笔，而不是假装支持任意分辨率。
func sizeToRatio(size string) string {
	s := strings.ToLower(strings.TrimSpace(size))
	if s == "" {
		return ""
	}
	if s == "auto" {
		return "auto"
	}
	if strings.Contains(s, ":") {
		switch s {
		case "1:1", "3:4", "4:3", "16:9", "9:16":
			return s
		}
		return ""
	}
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return ""
	}
	// 交叉相乘判比例：w/h == a/b  ⇔  w*b == h*a
	switch {
	case w == h:
		return "1:1"
	case w*3 == h*4: // 4:3
		return "4:3"
	case w*4 == h*3: // 3:4
		return "3:4"
	case w*9 == h*16: // 16:9
		return "16:9"
	case w*16 == h*9: // 9:16
		return "9:16"
	}
	// 最接近的比例（按宽高比大小）。
	r := float64(w) / float64(h)
	cand := []struct {
		name string
		val  float64
	}{{"1:1", 1}, {"4:3", 4.0 / 3}, {"3:4", 0.75}, {"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}}
	best, bestDiff := "", 1e9
	for _, c := range cand {
		d := r - c.val
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			best, bestDiff = c.name, d
		}
	}
	return best
}

// buildImagePrompt 处理 prompt。
//
// 🔴 2026-10-28 实测订正：**什么都不要加**。t2i 是显式类型（chat_type + extra.meta.model
// 已经声明"这是画图任务"），上游按类型分派，不看提示词措辞。
// 早先版本给 prompt 加「请画一张图：」前缀 + 画幅后缀，结果上游只回
// `response.created` + keep_alive、一个内容帧都没有（200s 空转）——
// 因为 t2i 分支拿到的是"一段被改写的文本"，而不是绘图提示词。
// 画幅已经在 extra.meta.size 与顶层 size 里，写进 prompt 反而是噪音。
func buildImagePrompt(prompt, ratio string) string {
	return strings.TrimSpace(prompt)
}

// handleImages `POST /v1/images/generations`。
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID: "img-" + upstream.UUID()[:8], Path: "/v1/images/generations",
		ClientIP: clientIP(r), UserAgent: truncateUA(r.UserAgent()),
	}

	var req ImageRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "解析请求体: " + err.Error()
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	refs, refErr := req.imageRefs()
	if refErr != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = refErr.Error()
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", refErr.Error(), "image")
		return
	}
	// 图生图：参考图先传到上游自有 OSS，再以 image_edit 提交（与生成必须同一个账号）。
	if len(refs) > maxInputImages {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("image 最多 %d 张", maxInputImages), "image")
		return
	}
	if maskWithoutImage(req.Mask, refs) {
		entry.Status = http.StatusBadRequest
		entry.Error = "mask 没有 image"
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "mask 必须配合 image 使用", "mask")
		return
	}
	// 上游没有 inpaint 语义：带图时 mask 收下但不转发（文档契约）。
	// 不静默 —— 日志留痕，免得以后有人以为"mask 生效了"。
	if strings.TrimSpace(req.Mask) != "" {
		log.Printf("[qwen2api] 图片请求带了 mask，但上游无 inpaint 语义，已忽略")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		entry.Status = http.StatusBadRequest
		entry.Error = "prompt 为空"
		s.addReq(&entry)
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			"prompt 不能为空（无 image 时必填）", "prompt")
		return
	}
	// n：非法值归一为 1（对齐 image-adapter 的"能修就修"）；上限 4 是因为
	// 每张都是一次独立的上游调用 + 一次配额消耗，不设上限等于允许一次烧光额度。
	if req.N <= 0 {
		req.N = 1
	}
	if req.N > 4 {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			"n 最大 4（每张是一次独立的上游调用，会分别消耗配额）", "n")
		return
	}
	// response_format：不认识的值视为"未指定"，用默认 url（不报错）。
	format := strings.ToLower(strings.TrimSpace(req.ResponseFormat))
	if format != "url" && format != "b64_json" {
		format = "url"
	}

	// model 字段可能填 chat 模型，也可能填图片模型（清单里两者都在）。
	requested := strings.TrimSpace(req.Model)
	model, imageModel := s.resolveImageModel(requested)
	if requested == "" {
		requested = imageModel
	}
	// 图片默认走 fast（关思考）：绘图任务的思考只会拖时间，不改善画面。
	// 请求里显式给了 thinking 才用它的。
	thinking := "fast"
	if v := strings.TrimSpace(req.Thinking); v != "" {
		thinking = v
	}
	ratio := sizeToRatio(req.Size)
	prompt := buildImagePrompt(req.Prompt, ratio)

	entry.Model = requested
	entry.RefImages = len(refs)
	// 图片是计费动作：n>1 时逐张串行，不并发轰同一账号。
	var (
		datums   []ImageDatum
		lastErr  error
		usedAcct string
	)
	for i := 0; i < req.N; i++ {
		imgs, revised, acct, err := s.generateImage(r.Context(), model, imageModel, thinking, prompt, refs, ratio, s.blobBase(r))
		// 🔴 失败也要记账号：否则流水里 acct= 空，既定位不到号、也带不出出口 IP
		// （2026-10-30 线上出现过一条 acct= 空的 502，白查了一轮）。
		usedAcct = acct
		if err != nil {
			lastErr = err
			break
		}
		for _, u := range imgs {
			d := ImageDatum{URL: u, RevisedPrompt: revised}
			if format == "b64_json" {
				b64, derr := downloadAsBase64(r.Context(), u)
				if derr != nil {
					// 直链拿不到就退回 url —— 绝不返回空 b64 让调用方存下一堆空文件。
					log.Printf("[qwen2api] 图片转 base64 失败（改回 url）: %v", derr)
				} else {
					d.B64JSON = b64
					d.URL = ""
				}
			}
			datums = append(datums, d)
		}
	}

	if len(datums) == 0 {
		code := httpStatusFor(lastErr)
		if lastErr == nil {
			lastErr = errors.New("上游没有返回任何图片（既没有 image_gen 产物帧，" +
				"也没有额度提示 —— 可能是模型判定该请求不需要绘图，或上游形态又变了；" +
				"建议在 prompt 里明确写「画…」）")
			code = http.StatusBadGateway
		}
		entry.Account = usedAcct
		entry.Status = code
		entry.Error = lastErr.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(&entry)
		writeError(w, code, errTypeFor(lastErr), lastErr.Error())
		return
	}

	writeJSON(w, http.StatusOK, ImageResponse{Created: time.Now().Unix(), Data: datums})

	entry.Account = usedAcct
	entry.Status = http.StatusOK
	entry.Chars = len(datums)
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(&entry)
}

// maxInputImages 单次图生图的参考图上限（上游单请求的实务上限；再多请分批）。
const maxInputImages = 20

// errUpstreamTimeout 上游在时间预算内没有任何产物（只发心跳）。
//
// 单独做成 sentinel 是为了让上层能把它映射成 **504 + 标准英文文案**：
// "额度将尽 or 上游抖动"这种判断只对运维有意义，对调用方只剩"过会儿再来"。
var errUpstreamTimeout = errors.New("上游在时间预算内没有任何产物（只发心跳）")

// imageTimeout 单次出图的时间预算（按参考图数量放宽）。
//
// 依据（2026-10-30 生产实测）：1 张 47.8s / 2 张 69.9s / 3 张 83.4s。
// 基线 180s 覆盖无参考图与单图；每多一张加 30s（实测增量 18s + 抖动余量），
// 上限 300s —— nginx 那边 900s，不会抢在网关前面回 504。
func imageTimeout(refs int) time.Duration {
	d := 180 * time.Second
	if refs > 1 {
		d += time.Duration(refs-1) * 30 * time.Second
	}
	if d > 300*time.Second {
		d = 300 * time.Second
	}
	return d
}

// generateImage 跑一次图片生成：选号 → （有参考图时）上传 → 建会话 → 提交 → 读流。
//
// 🔴 上传与生成**必须同一个账号**：file_id 是账号维度的资源，
// 用 A 号传的图让 B 号去生成，上游会当成"无权访问该文件"。
// imageModel 是"图片内核"（extra.meta.model），与建会话用的 chat 模型分开：
// 上游模型表里没有图片模型 id，把它当 chat 模型发会 Bad_Request。
func (s *Server) generateImage(ctx context.Context, model, imageModel, thinking, prompt string,
	refs []string, ratio, blobBase string) (imgs []string, revised, acct string, err error) {

	// 🔴 上游额度用尽后不会报错，而是**只发 keep_alive 心跳、永不产出**（2026-10-28 实测：
	// 第 4 张起 240s 无产物帧）。所以必须由网关自己设上限，否则调用方只能干等到客户端超时。
	// 预算按垫图数量放宽：多一张参考图上游确实更慢（实测 1 张 47.8s / 2 张 69.9s /
	// 3 张 83.4s，≈ 每张 +18s），而原来"180s 含上传"的推导已经过时 —— 上传现在是本地
	// 内存写 blob，真正的耗时全在生成。
	budget := imageTimeout(len(refs))
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// 🔴 用 PickForImage：跳过"今日已出够 max_image_per_day 张"的号。
	// 上游打到硬限不会报错，只会静默挂起 180s —— 本地配额就是为了不去撞那堵墙。
	a, ok := s.pool.PickForImage()
	if ok {
		s.egressOf(a.Name) // 同 chat：先起探测，出图期间足够探完
	}
	if !ok {
		// 区分"全池配额用尽"与"全部冷却/失效"，否则调用方不知道该等还是该加号。
		if left := s.pool.ImageQuotaLeft(); left == 0 {
			used, limit := s.pool.ImageUsage(nil)
			_ = used
			return nil, "", "", fmt.Errorf("%w：全部 %d 个账号今日出图额度已用尽（本地限额 %d 张/号/天）",
				errImageQuotaLocal, s.pool.Len(), limit)
		}
		return nil, "", "", errNoAccount
	}
	defer s.pool.Release(a)
	cred := s.pool.Credential(a.Name)
	// 上传与生成必须同一条出口，否则"传的时候一个 IP、画的时候另一个 IP"
	// 会让上游把文件判成跨账号资源。
	ctx = upstream.WithAccount(ctx, a.Name)

	var spec upstream.ChatSpec
	if len(refs) > 0 {
		// 🔴 参考图走 URL 直通 / 本地 blob，**不再过 getstsToken**：
		// 该端点已被阿里云 WAF 滑块挡住（docs/UPSTREAM.md §6.1），而
		// image_edit 的外链实测可用 —— 2026-10-30 端到端验证：
		// 白底黑字 HELLO42 参考图 + "背景改蓝、保留文字" ⇒ 产物 2752×1376
		// 蓝底黑字 HELLO42（宽高比与原图一致，证明是真编辑不是 t2i 重画）。
		files, upErr := s.visionFiles(ctx, blobBase, refs)
		if upErr != nil {
			s.noteFailure(a, upErr)
			return nil, "", a.Name, upErr
		}
		spec = upstream.ImageEditSpec(files)
	} else {
		// 🔴 extra.meta.model 是关键：不传图片模型时上游只是"聊图"，画不出来。
		spec = upstream.ImageSpec(imageModel, ratio)
	}

	chatID, err := s.up.NewChatTyped(ctx, cred, model, spec.ChatType)
	if err != nil {
		s.noteFailure(a, err)
		return nil, "", a.Name, err
	}
	body := upstream.BuildSubmitSpec(chatID, "", model, prompt, thinking, spec, time.Now())
	st, err := s.up.OpenChat(ctx, cred, body)
	if err != nil {
		s.noteFailure(a, err)
		return nil, "", a.Name, err
	}
	defer st.Close()

	for {
		ev, more, err := st.Next()
		if err != nil {
			s.noteFailure(a, err)
			if ctx.Err() == context.DeadlineExceeded {
				// ⚠️ 挂起是**双义**的（2026-10-28 实测两种都遇到）：
				//   · 额度将尽的前兆 —— 再发一次会拿到明确的 insufficient_quota；
				//   · 上游临时故障 —— 等几分钟同一账号又能出图（本号在第 4、5 次挂起后恢复）。
				// 所以文案不能断言"额度用完"，只能说"两种可能 + 怎么区分"。
				// 🔴 这段中文只进流水/容器日志；客户端拿到的是 504 + "upstream timeout"
				// （见 clientError）—— 排查指南不该出现在 API 响应里。
				return nil, "", a.Name, fmt.Errorf("%w（预算 %s，垫图 %d 张）。两种可能："+
					"①该账号今日生图额度将尽（再发一次通常返回 insufficient_quota 确认）；"+
					"②上游临时故障（等几分钟重试往往恢复，多垫图时更慢）。建议换号或稍后重试",
					errUpstreamTimeout, budget, len(refs))
			}
			return nil, "", a.Name, err
		}
		if !more {
			break
		}
		if ev.QuotaExhausted {
			// 上游把"额度用完"当 answer 下发。必须立刻转成明确错误 ——
			// 否则会落到"没图"分支，报成"模型没画"，把调用方引向改 prompt 的错方向。
			msg := ev.QuotaMessage
			if msg == "" {
				msg = "上游额度已用完（未给出具体文案）"
			}
			s.pool.MarkOK(a)
			return nil, "", a.Name, fmt.Errorf("%w：%s", upstream.ErrQuota, msg)
		}
		if ev.ImagePrompt != "" {
			revised = ev.ImagePrompt
		}
		if len(ev.Images) > 0 {
			imgs = append(imgs, ev.Images...)
		}
		if ev.Done {
			// 图片帧在 answer 之前到，Done 之后不会再有图。
			break
		}
	}
	s.pool.MarkOK(a)
	if len(imgs) > 0 {
		// 只记成功张数：挂起/失败不占额度（实测挂起不扣上游配额）。
		s.pool.MarkImageDone(a)
	}
	return imgs, revised, a.Name, nil
}

// downloadAsBase64 把直链图片转成 base64（b64_json 用）。
//
// 🔴 两个实测坑（2026-10-28）：
//  1. **刚生成的那几秒 CDN 还没转存完** —— 立刻下会拿到 404 + 226 字节 HTML 死链。
//     所以带退避重试（1s/3s），而不是一次失败就判死。
//  2. 下载要带 `Referer: https://chat.qwen.ai/`（视频产物更是必需，图片有时也需要）。
//     不带的话同样是 404，看起来像"图不存在"，其实是防盗链。
//
// 另外按 image-adapter 的教训：**判真假看 magic bytes，不看 Content-Type**
// （真图可能回 application/octet-stream，按 image/* 判会误杀）。
func downloadAsBase64(ctx context.Context, url string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		raw, err := fetchMedia(ctx, url)
		if err == nil {
			return base64.StdEncoding.EncodeToString(raw), nil
		}
		lastErr = err
	}
	return "", lastErr
}

// fetchMedia 下载一个媒体直链（带 Referer/UA，按魔数校验是不是真媒体）。
func fetchMedia(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", "https://chat.qwen.ai/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载媒体 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 40<<20))
	if err != nil {
		return nil, err
	}
	if !looksLikeMedia(raw) {
		// 典型死链：226 字节 HTML。给出可诊断的信息，而不是把 HTML 当图返回。
		return nil, fmt.Errorf("下载到的不是媒体（前 16 字节 %q，共 %d 字节）",
			string(raw[:min(16, len(raw))]), len(raw))
	}
	return raw, nil
}

// looksLikeMedia 按魔数判断 PNG/JPEG/GIF/WEBP/MP4。
func looksLikeMedia(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	switch {
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G': // PNG
		return true
	case b[0] == 0xFF && b[1] == 0xD8: // JPEG
		return true
	case b[0] == 'G' && b[1] == 'I' && b[2] == 'F': // GIF
		return true
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return true
	case string(b[4:8]) == "ftyp": // MP4
		return true
	}
	return false
}

// handleImageEdits `POST /v1/images/edits`。
//
// 与 generations 同一条生成链，只是载体是 multipart：`image` / `image[]` 收文件，
// 也收文本形态的 `image=<URL>`。参考图同样走 getstsToken → OSS V4 签名 PUT 上传，
// 再以 chat_type=image_edit 提交（上传与生成必须同一个账号）。
//
// 没收到参考图时**直接 400**，不退化成文生图 —— "偷偷把图丢掉当 t2i 跑"
// 是最坏的欺骗：调用方会以为编辑成功。
func (s *Server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{
		ID: "edit-" + upstream.UUID()[:8], Path: "/v1/images/edits",
		ClientIP: clientIP(r), UserAgent: truncateUA(r.UserAgent()),
	}

	prompt, refs, err := readMultipartImages(r)
	if err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = err.Error()
		s.addReq(&entry)
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if prompt == "" {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", "prompt 不能为空", "prompt")
		return
	}
	if len(refs) == 0 {
		// edits 语义上必须带图；没带就明说，别悄悄退化成文生图。
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			"没有收到参考图（multipart 字段 image 或 image[]）", "image")
		return
	}
	if len(refs) > maxInputImages {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("image 最多 %d 张", maxInputImages), "image")
		return
	}

	requested := strings.TrimSpace(r.FormValue("model"))
	model, imageModel := s.resolveImageModel(requested)
	if requested == "" {
		requested = imageModel
	}
	thinking := "fast"
	format := strings.ToLower(strings.TrimSpace(r.FormValue("response_format")))
	if format != "url" && format != "b64_json" {
		format = "url"
	}

	entry.RefImages = len(refs)
	imgs, revised, acct, genErr := s.generateImage(r.Context(), model, imageModel, thinking, prompt, refs, "", s.blobBase(r))
	if genErr != nil {
		code := httpStatusFor(genErr)
		entry.Account = acct
		entry.Status = code
		entry.Error = genErr.Error()
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(&entry)
		writeError(w, code, errTypeFor(genErr), genErr.Error())
		return
	}
	if len(imgs) == 0 {
		entry.Account = acct
		entry.Status = http.StatusBadGateway
		entry.Error = "上游没有返回任何图片"
		entry.DurationMS = time.Since(start).Milliseconds()
		s.addReq(&entry)
		writeError(w, http.StatusBadGateway, "upstream_error",
			"上游没有返回任何图片（模型未触发 image_edit 产物帧）")
		return
	}

	datums := make([]ImageDatum, 0, len(imgs))
	for _, u := range imgs {
		d := ImageDatum{URL: u, RevisedPrompt: revised}
		if format == "b64_json" {
			if b64, derr := downloadAsBase64(r.Context(), u); derr == nil {
				d.B64JSON = b64
				d.URL = ""
			}
		}
		datums = append(datums, d)
	}
	writeJSON(w, http.StatusOK, ImageResponse{Created: time.Now().Unix(), Data: datums})

	entry.Model = requested
	entry.Account = acct
	entry.Status = http.StatusOK
	entry.Chars = len(datums)
	entry.DurationMS = time.Since(start).Milliseconds()
	s.addReq(&entry)
}
