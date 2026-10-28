package upstream

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// HeaderVersion 与前端版本号自洽：实测 0.3.26（2026-10-28）。
// 旧版本号 0.2.0 在新鉴权形态下不再适用（见 docs/UPSTREAM.md §2.2）。
const HeaderVersion = "0.3.26"

// buildHeaders 构造上游请求头 —— 逐字段对齐实测抓包。
//
// 🔴 为什么这么较真：这套头是"像不像浏览器"的唯一门面。缺 Sec-Fetch-* /
// timezone / source 会被判自动化（RGV587 风控），而这个错误的表象是
// "请求体不合法"，极易误诊。2026-10-28 复核：网页端自己也带不全
// bx-ua/bx-umidtoken（值是 "defaultFY2_load_failed with timeout"），
// 说明这两项不是硬门槛 —— 但其余字段一个都不能少。
func (c *Client) buildHeaders(cred, referer string) http.Header {
	return c.buildHeadersFor(cred, referer, false)
}

// buildHeadersFor 构造请求头。crossSite=true 时用于 auth 域：
//
// 🔴 实测（2026-10-28）：`auth.qwen.ai/api/v2/auths/refresh` 额外要求
//   - `x-request-origin: https://chat.qwen.ai`（少了 ⇒ `Bad_Request: Missing origin`）
//   - `sec-fetch-site: same-site`（写成 same-origin ⇒ `Invalid request header`）
//
// 这两条是 CORS 白名单（§2.3）里唯一"必须由调用方显式补"的。
func (c *Client) buildHeadersFor(cred, referer string, crossSite bool) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Content-Type", "application/json")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("User-Agent", c.cfg.UserAgent)
	h.Set("Origin", c.cfg.BaseURL)
	if referer == "" {
		referer = c.cfg.BaseURL + "/"
	}
	h.Set("Referer", referer)
	h.Set("source", "web")
	h.Set("version", c.cfg.Version)
	h.Set("Timezone", TimezoneHeader(time.Now()))
	h.Set("X-Request-Id", UUID())
	// 🔴 页面在 completions 上**显式设了这 8 个头**（2026-10-28 hook 实证：
	// Accept-Language / X-Accel-Buffering / X-Request-Id / Accept / Content-Type /
	// version / source / timezone）。X-Accel-Buffering 最容易漏 —— 它是给
	// 中间层（nginx）的"别缓冲"指令，缺了不像会出事，但风控画像会不一样。
	h.Set("X-Accel-Buffering", "no")
	h.Set("bx-v", "2.5.37")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	if crossSite {
		h.Set("Sec-Fetch-Site", "same-site")
		h.Set("x-request-origin", c.cfg.BaseURL)
	} else {
		h.Set("Sec-Fetch-Site", "same-origin")
	}
	h.Set("sec-ch-ua", `"Not:A-Brand";v="8", "Chromium";v="156", "Google Chrome";v="156"`)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", `"macOS"`)

	// 🔴 这两个头是能不能过的分水岭（2026-10-28 实测）：
	// 缺它们 ⇒ HTTP 200 + `RGV587` x5sec 风控（表象是"请求被拒"，极易误判成凭据问题）；
	// 按网页端原样的 "load_failed" 形态发 ⇒ 正常返回。值看起来像"加载失败的兜底"，
	// 但服务端只认这个形状，不是真的指纹也算数。
	h.Set("bx-ua", c.bxUA())
	h.Set("bx-umidtoken", c.bxUA())

	mode, value := SplitCredential(cred)
	switch {
	case value == "":
		// 匿名（公共端点）—— 不带任何凭据。
	case mode == ModeCookie:
		// 完整 jar 原样发；裸 token 包成 token=<jwt>。
		if IsCookieJar(value) {
			h.Set("Cookie", value)
		} else {
			h.Set("Cookie", "token="+value)
		}
	default:
		// mode==ModeBearer，或未探测（mode==""）—— 裸 token 默认按 Bearer 发。
		// 未探测时由上层负责先探一次（DetectAuthMode），否则会拿到 401。
		h.Set("Authorization", "Bearer "+value)
	}
	return h
}

// bxUA 生成 bx-ua / bx-umidtoken 的值。
//
// 形如：`defaultFY2_load_failed with timeout@@https://chat.qwen.ai@@1793180907875`
// 末段是毫秒时间戳（每次请求都变）。配置里写了自定义值就优先用它。
func (c *Client) bxUA() string {
	if c.cfg.BxUA != "" {
		return c.cfg.BxUA
	}
	return fmt.Sprintf("defaultFY2_load_failed with timeout@@%s@@%d",
		c.cfg.BaseURL, time.Now().UnixMilli())
}

// StreamReferer 流式/提交请求的 referer 指向具体会话页（实测如此）。
func (c *Client) chatReferer(chatID string) string {
	if chatID == "" {
		return c.cfg.BaseURL + "/"
	}
	return c.cfg.BaseURL + "/c/" + chatID
}

// actualStatus 读取上游 x-actual-status-code 响应头 —— 该站查询与流式端点
// HTTP 状态恒 200，真码在这个头里。
func actualStatus(resp *http.Response) int {
	raw := resp.Header.Get("x-actual-status-code")
	if raw == "" {
		return resp.StatusCode
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return resp.StatusCode
	}
	return n
}
