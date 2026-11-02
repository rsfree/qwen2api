package server

// 图片直链代理：由网关替调用方补上防盗链要求的头，**实时透传**上游 CDN 的
// 字节（不落盘、不转存）。
//
// ⚠️ 当前状态（2026-11-02 用户决定）：`url` 字段**已改回原样返回上游直链**
// （cdn.qwenlm.ai），不再改写成网关域名 —— 见 images.go 调用处的说明。
// 本端点仍然保留并可用（`GET /v1/media/output/...`），因为它是目前唯一能
// 绕过防盗链的通路：只要把 images.go 里的 `ImageDatum{URL: u}` 换成
// `proxyImageURL(s.blobBase(r), u)`，响应就切回网关地址，本文件不用动。
// 保留而不是删掉的理由：改回来一行的事，删了将来要重写并重测。
//
// 🔴 为什么这个端点有意义（2026-11-02 用户直接点开链接 403 时实测）：
// 上游产物直链 cdn.qwenlm.ai 前面是阿里云 CDN + Tengine，有**两道**彼此独立的防盗链：
//
//	| 请求特征                        | 结果                                    |
//	|---------------------------------|-----------------------------------------|
//	| 浏览器 UA + 无/空 Referer        | 200（地址栏直输属于这一类）              |
//	| 浏览器 UA + Referer=chat.qwen.ai | 200                                     |
//	| 浏览器 UA + Referer=任何第三方站  | 403 denied by Referer ACL               |
//	| curl / python / SDK 默认 UA      | 404 + 226 字节 HTML（伪装成"文件不存在"）|
//
// 于是原链只要被网页 <img> 引用（浏览器自动带页面 Referer）或被服务端 SDK 拉取，
// 就**必然**失败。本端点替调用方补上合规的 UA/Referer，再把字节流回去。
//
// 为什么是透传而不是转存：上游链接本身约 2 天有效，转存要额外承担磁盘与清理，
// 而当前要解决的是"能不能访问"，不是"能不能长期保存"。
//
// 安全边界（这个端点**必须免鉴权**：<img> 标签不会带我们的 API Key）：
//   - 上游 host 硬编码，只认 cdn.qwenlm.ai ⇒ 天然免疫 SSRF，绝不接受调用方指定目标；
//   - path 必须以 /output/ 开头且不含 ".."（上游产物的固定形态）；
//   - 只允许 GET/HEAD，只回媒体字节，不回任何元数据；
//   - 访问控制交给原链里的 key（不可猜的 JWT）：能拼出链接才取得到图，
//     本端点不放大任何权限，也枚举不出别人的图。

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

const (
	// mediaUpstreamHost 唯一允许代理的上游（写死，不读配置）。
	mediaUpstreamHost = "cdn.qwenlm.ai"
	// mediaProxyPath 代理端点的前缀，拼在 blobBase 后面。
	mediaProxyPath = "/v1/media"
	// mediaReferer 上游 Referer 白名单内的值（实测 chat.qwen.ai 与空 Referer 都放行，
	// 这里用前者：语义上更"像自家页面"，也避免个别节点对空 Referer 收紧）。
	mediaReferer = "https://chat.qwen.ai/"
	// mediaMaxBytes 单个响应上限，与 fetchMedia 同口径。
	mediaMaxBytes = 40 << 20
	// mediaCacheS 下游缓存时长。必须**小于**上游链接寿命（约 2 天），
	// 又不能太短 —— 同一张图会在页面里反复加载。
	mediaCacheS = 86400
	// mediaTimeout 单次拉取的兜底上限（大图 + 慢客户端）。
	mediaTimeout = 5 * time.Minute
)

// mediaClient 专用于媒体透传：不设整体 Timeout（会误杀大图），靠 ctx 控时。
//
// Proxy 跟随环境变量（与 fetchMedia 用的 http.DefaultClient 同语义），
// 避免"某些部署必须走代理才能出网"时这里成为唯一断点。
var mediaClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
		// 🔴 必须关掉：Go 默认会自动加 Accept-Encoding: gzip 并**悄悄解压**，
		// 同时把 ContentLength 置 -1、删掉 Content-Encoding。
		// 那样透传出去的字节虽然还能看，但 Content-Length 与实际不一致，
		// 某些客户端会按声明长度截断。这里要的是字节原样。
		DisableCompression: true,
	},
}

// proxyImageURL 把上游直链改写成网关代理地址。
//
// ⚠️ 当前**没有生产调用者**（只被测试引用）：2026-11-02 用户要求 url 全部返回
// 上游原链，所以 images.go 两处调用已撤回。函数留着是为了"切回代理模式"只需
// 改 images.go 那一行，不必重写这段路径校验与转义逻辑。
//
// 任何"不认识/改写不了"的情况都**原样返回**：宁可退回一个可能 403 的原链，
// 也不要返回一个我们自己也拉不到的相对路径（那会让调用方拿到 404 而无法归因）。
func proxyImageURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if raw == "" || base == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") ||
		!strings.EqualFold(u.Host, mediaUpstreamHost) {
		return raw
	}
	p := u.EscapedPath()
	if !strings.HasPrefix(p, "/output/") || strings.Contains(p, "..") {
		return raw
	}
	out := base + mediaProxyPath + p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// handleImageProxy `GET /v1/media/output/...` —— 免鉴权的图片透传。
//
// 路径直接沿用上游的 /output/... 形态（而不是把整条 URL 塞进 query）：
// 短、可读、和原链一一对应，也省掉一层转义。
func (s *Server) handleImageProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 用 EscapedPath：Path 已被解码，%2F 之类的还原后再拼回去语义就变了。
	p := strings.TrimPrefix(r.URL.EscapedPath(), mediaProxyPath)
	if !strings.HasPrefix(p, "/output/") || strings.Contains(p, "..") {
		http.NotFound(w, r)
		return
	}
	target := "https://" + mediaUpstreamHost + p
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	s.passThroughMedia(w, r, target)
}

// passThroughMedia 拉上游并流式转发。target 已由 handleImageProxy 校验过，
// 本函数不再做 host 判断（单测直接喂 httptest 地址）。
func (s *Server) passThroughMedia(w http.ResponseWriter, r *http.Request, target string) {
	ua := s.conf().Upstream.UserAgent
	if ua == "" {
		ua = config.Default().Upstream.UserAgent
	}

	var (
		resp *http.Response
		err  error
	)
	// 重试只针对 404：刚出图的那几秒 CDN 还没转存完，立刻拉就是 404
	// （与 downloadAsBase64 同一个坑）。真失效（约 2 天后）也是 404，
	// 所以只多等一次 1.2s，不做那边的 1s/3s 三级退避 —— 这里面对的是
	// 浏览器 <img>，拖太久只会让页面白等。
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(1200 * time.Millisecond):
			}
		}
		resp, err = fetchMediaStream(r, target, ua)
		if err == nil && resp.StatusCode == http.StatusNotFound && attempt == 0 {
			resp.Body.Close()
			continue
		}
		break
	}
	if err != nil {
		if r.Context().Err() != nil {
			return // 客户端自己断了，没人收响应
		}
		log.Printf("[qwen2api] 图片透传失败 %s: %v", target, err)
		http.Error(w, "upstream media unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		// 正常路径
	case http.StatusNotFound:
		// 两种可能：刚生成还没转存（已重试过），或链接过期（约 2 天）。
		// 对 <img> 来说都是"没图"，但运维需要能区分，所以留一行日志。
		log.Printf("[qwen2api] 图片已失效（404）：%s", target)
		http.Error(w, "upstream image expired or not yet stored", http.StatusNotFound)
		return
	case http.StatusForbidden:
		// 我们自己带对了 UA/Referer 仍被拒 ⇒ 出口 IP 被 WAF 拦，属于网关侧故障。
		log.Printf("[qwen2api] 图片透传被上游拒绝（403）：%s", target)
		http.Error(w, "upstream cdn denied this egress", http.StatusBadGateway)
		return
	default:
		log.Printf("[qwen2api] 图片透传上游返回 %d：%s", resp.StatusCode, target)
		http.Error(w, "upstream media error", http.StatusBadGateway)
		return
	}

	h := w.Header()
	if ct := resp.Header.Get("Content-Type"); ct != "" && strings.HasPrefix(strings.ToLower(ct), "image/") {
		h.Set("Content-Type", ct)
	} else {
		// 上游偶尔给 application/octet-stream（见 fetchMedia 的注释），
		// 按扩展名兜底，免得浏览器把它当附件下载。
		h.Set("Content-Type", mediaMIME(r.URL.Path))
	}
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	// DisableCompression=true 意味着压缩字节是原样转发的，编码头必须跟着走，
	// 否则客户端会把 gzip 流当明文解析。
	if v := resp.Header.Get("Content-Encoding"); v != "" {
		h.Set("Content-Encoding", v)
	}
	if v := resp.Header.Get("Content-Range"); v != "" {
		h.Set("Content-Range", v)
	}
	// 不设 Content-Disposition：让浏览器内联渲染。
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(mediaCacheS))
	// 前端 canvas 取像素需要 CORS（<img> 本身不需要）。
	h.Set("Access-Control-Allow-Origin", "*")

	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	// 客户端中途断开（换页/取消）是常态，不值得记日志。
	_, _ = io.Copy(w, io.LimitReader(resp.Body, mediaMaxBytes))
}

// fetchMediaStream 发起一次媒体请求（不读 body，留给调用方流式转发）。
func fetchMediaStream(r *http.Request, target, ua string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(r.Context(), mediaTimeout)
	// 🔴 上游对 HEAD 只回 `content-length: 0`（阿里云 OSS 行为，2026-11-02 实测），
	// 直接透传会让探测方以为图是空的。所以一律用 GET 取 header，
	// HEAD 的差异由调用方处理（不写 body 而已）。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	// 🔴 这两个头就是"能不能拿到图"的全部关键，缺一个分别是 404 / 403
	// （见文件头表格，2026-11-02 实测）。
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", mediaReferer)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	// Range 透传：图片一般用不到，但视频帧/断点续传会用到，成本为零。
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	resp, err := mediaClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	// cancel 必须跟着 body 一起结束：提前调用会掐断正在读的流。
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose 让 ctx 的 cancel 跟随响应体关闭。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// mediaMIME 按扩展名兜底推断类型（只覆盖上游会产出的几种）。
func mediaMIME(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}
