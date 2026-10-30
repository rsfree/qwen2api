package server

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// mediaModelCreated 生成模型条目的 created 时间戳（固定值）。
//
// 用常量而不是 time.Now()：清单要稳定 —— created 每次都变会让按它排序/判重的
// 客户端反复失效（有些客户端把 created 当版本号看）。
const mediaModelCreated = 1761782400 // 2026-10-30 00:00:00 UTC

// mediaModels 把「生成模型」（图片 / 视频）作为独立条目列进 /v1/models。
//
// 🔴 上游**没有**独立的图片/视频模型 id —— 2026-10-30 实测 `GET /api/v2/models/`
// 只有 7 个 chat 模型，图片靠 `extra.meta.model`（qwen-image-3.0）选内核，
// 视频靠 `biz_data.rootModel`（happyhorse11）选内核。
//
// 所以这里列的是**本网关的生成模型名**：调用方在请求的 model 字段传它，
// 网关负责翻译成上游那两个字段（见 resolveImageModel / resolveVideoModel）。
// 清单必须与实际能力一一对应 —— 列一个用不了的名字，比不列更糟。
func (s *Server) mediaModels() []ModelInfo {
	out := make([]ModelInfo, 0, 2)

	if id := strings.TrimSpace(s.conf().ImageModel); id != "" {
		out = append(out, ModelInfo{
			ID:       id,
			Object:   "model",
			Created:  mediaModelCreated,
			OwnedBy:  "qwen",
			Title:    "Qwen-Image 3.0",
			Media:    "image",
			Endpoint: "/v1/images/generations",
			Verified: true,
			Params: map[string]any{
				"sizes": []string{"1:1", "4:3", "3:4", "16:9", "9:16"},
				"max_n": 4,
			},
			Notes: "平衡质量与速度。按张计费（1 张 = 1 积分）；带 image 字段即图生图（也可用 /v1/images/edits）。",
		})
	}

	if id := strings.TrimSpace(s.conf().Video.RootModel); id != "" {
		params := map[string]any{
			"sizes": []string{"16:9", "9:16", "1:1"},
			"audio": []bool{true, false},
		}
		if pricing := s.conf().Video.Pricing; len(pricing) > 0 {
			resolutions := make([]string, 0, len(pricing))
			durSet := map[int]struct{}{}
			for res, byDur := range pricing {
				resolutions = append(resolutions, res)
				for ds := range byDur {
					if n, err := strconv.Atoi(ds); err == nil {
						durSet[n] = struct{}{}
					}
				}
			}
			durations := make([]int, 0, len(durSet))
			for n := range durSet {
				durations = append(durations, n)
			}
			sort.Ints(durations)
			sort.Slice(resolutions, func(i, j int) bool {
				return resolutionRank(resolutions[i]) < resolutionRank(resolutions[j])
			})
			params["resolutions"] = resolutions
			params["durations"] = durations
			params["credits"] = pricing
		}
		out = append(out, ModelInfo{
			ID:       id,
			Object:   "model",
			Created:  mediaModelCreated,
			OwnedBy:  "qwen",
			Title:    videoTitle(id),
			Media:    "video",
			Endpoint: "/v1/videos/generations",
			Verified: true,
			Params:   params,
			Notes: "更自然稳定。异步任务：提交拿 id，再 GET /v1/videos/generations/{id} 轮询。" +
				videoPriceNote(s.conf().Video.Pricing),
		})
	}

	return out
}

// videoTitle 视频模型标题。
//
// 🔴 不写死 "HappyHorse 1.1"：root_model 是可配置的（换 1.0 时价格差 33%~75%），
// 写死会让清单变成一个骗人的名字。认不出来的直接回 id —— 宁可朴素，不可撒谎。
func videoTitle(id string) string {
	switch k := modelKey(id); {
	case k == "happyhorse11":
		return "HappyHorse 1.1"
	case strings.HasPrefix(k, "happyhorse"):
		return "HappyHorse"
	}
	return id
}

// videoPriceNote 从价格表生成"720P 5s=30 / 1080P 15s=120"这样的说明。
//
// 同样不写死数字：pricing 换成 hh（1.0）后是 40/210，写死的文案会让人按错的
// 价格判断成本。表为空时明说"按个数限额"，而不是留一句空话。
func videoPriceNote(pricing map[string]map[string]int) string {
	if len(pricing) == 0 {
		return "未配置 pricing，按每日个数限额。"
	}
	res := make([]string, 0, len(pricing))
	for r := range pricing {
		res = append(res, r)
	}
	sort.Slice(res, func(i, j int) bool { return resolutionRank(res[i]) < resolutionRank(res[j]) })

	// 取最小分辨率的"最短档"与最大分辨率的"最长档"：一头一尾最能代表价格区间。
	parts := make([]string, 0, 2)
	if d, c, ok := pickDuration(pricing[res[0]], false); ok {
		parts = append(parts, fmt.Sprintf("%s %ds=%d", res[0], d, c))
	}
	if len(res) > 1 {
		if d, c, ok := pickDuration(pricing[res[len(res)-1]], true); ok {
			parts = append(parts, fmt.Sprintf("%s %ds=%d", res[len(res)-1], d, c))
		}
	}
	if len(parts) == 0 {
		return "按档位扣积分，提交前校验账号余额。"
	}
	return "按档位扣积分（" + strings.Join(parts, " / ") + "），提交前校验账号余额。"
}

// pickDuration 在 resolution→秒→积分 的小表里取最短（longest=false）或最长档。
func pickDuration(byDur map[string]int, longest bool) (dur, credits int, ok bool) {
	for ds, c := range byDur {
		n, err := strconv.Atoi(ds)
		if err != nil {
			continue
		}
		if !ok || (longest && n > dur) || (!longest && n < dur) {
			dur, credits, ok = n, c, true
		}
	}
	return dur, credits, ok
}

// resolutionRank 分辨率排序权重：720P 在前、1080P 次之，其余按字典序殿后。
// 视频单价表是 map，不排序每次输出顺序都在跳，客户端看着像"模型一直在变"。
func resolutionRank(res string) int {
	switch strings.ToUpper(res) {
	case "720P":
		return 0
	case "1080P":
		return 1
	}
	return 2
}

// modelList 对外模型清单 = 上游 chat 模型 + 本网关的生成模型。
func (s *Server) modelList(ctx context.Context) []ModelInfo {
	list := s.models.List(ctx)
	return append(list, s.mediaModels()...)
}

// resolveImageModel 决定「用哪个 chat 模型建会话」与「用哪个图片内核」。
//
// 清单里同时有 chat 模型和图片模型，客户端可能把两者都填进 model 字段：
//   - 留空 / 填 chat 模型 ⇒ 内核用配置的 image_model（老行为）
//   - 填图片模型 id      ⇒ 内核用它；chat 会话仍用默认 chat 模型
//
// 🔴 不能把图片模型名当 chat 模型发给上游：`/api/v2/chats/new` 的 models[]
// 只认 chat 模型，填 qwen-image-3.0 会 Bad_Request —— 上游模型表里根本没这个 id。
func (s *Server) resolveImageModel(model string) (chatModel, imageModel string) {
	imageModel = strings.TrimSpace(s.conf().ImageModel)
	chatModel = strings.TrimSpace(model)
	if imageModel != "" && sameModelName(chatModel, imageModel) {
		chatModel = ""
	}
	if chatModel == "" {
		chatModel = s.conf().DefaultModel
	}
	if imageModel == "" {
		imageModel = "qwen-image-3.0"
	}
	return chatModel, imageModel
}

// resolveVideoModel 同上：返回建会话用的 chat 模型与生成内核 rootModel。
func (s *Server) resolveVideoModel(model string) (chatModel, rootModel string) {
	rootModel = strings.TrimSpace(s.conf().Video.RootModel)
	chatModel = strings.TrimSpace(model)
	if rootModel != "" && sameModelName(chatModel, rootModel) {
		chatModel = ""
	}
	if chatModel == "" {
		chatModel = s.conf().DefaultModel
	}
	if rootModel == "" {
		rootModel = "happyhorse11"
	}
	return chatModel, rootModel
}

// sameModelName 比较模型名时忽略大小写、空格与分隔符。
//
// 为什么要宽松：上游 UI 把图片模型显示成 "Qwen-Image 3.0"、视频显示成
// "HappyHorse 1.1"，而网关内部用的是 qwen-image-3.0 / happyhorse11
// （root_model 是上游 biz_data 里的取值，不能改）。用户从界面上抄名字过来
// 是常态，差一个空格就 400 属于无意义的刁难。
func sameModelName(a, b string) bool {
	ka, kb := modelKey(a), modelKey(b)
	return ka != "" && ka == kb
}

// modelKey 归一化：只留字母与数字，全部小写。
//
// 用 unicode 判定而不是 `a-z0-9`：模型名以后可能出现中文别名，
// 按 ASCII 过滤会把中文整段抹掉，任何两个中文名都会"相等"。
func modelKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// mediaEndpoint 该模型名是不是生成模型；是则返回应该调用的端点。
//
// chat 入口用它拦一句能指导处置的话。不拦的话模型名会被甩给上游，
// 换回一个 `Bad_Request` —— 调用方只会以为"网关坏了"。
func (s *Server) mediaEndpoint(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return ""
	}
	if id := strings.TrimSpace(s.conf().ImageModel); id != "" && sameModelName(m, id) {
		return "/v1/images/generations"
	}
	if id := strings.TrimSpace(s.conf().Video.RootModel); id != "" && sameModelName(m, id) {
		return "/v1/videos/generations"
	}
	return ""
}
