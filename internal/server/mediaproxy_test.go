package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- 图片直链代理 ----------
//
// 这组测试钉的是 2026-11-02 线上暴露的那条因果链：
// 上游 CDN 靠 UA + Referer 两道防盗链拦人，原链直接给调用方必然挂。
// 所以最关键的断言不是"能拿到字节"，而是**转发时带对了哪两个头**。

const testPNG = "\x89PNG\r\n\x1a\n" + "fake-png-payload-for-tests"

func newProxyServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	if mutate != nil {
		mutate(&cfg)
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test")
}

func TestProxyImageURL(t *testing.T) {
	const good = "https://cdn.qwenlm.ai/output/u1/t2i/c1/abc.png?key=k"
	cases := []struct {
		name string
		base string
		raw  string
		want string
	}{
		{
			name: "正常改写（query 原样保留）",
			base: "https://qwen2api.1task.cn",
			raw:  good,
			want: "https://qwen2api.1task.cn/v1/media/output/u1/t2i/c1/abc.png?key=k",
		},
		{
			name: "base 尾斜杠归一",
			base: "https://qwen2api.1task.cn/",
			raw:  good,
			want: "https://qwen2api.1task.cn/v1/media/output/u1/t2i/c1/abc.png?key=k",
		},
		{
			name: "无 query 时不留问号",
			base: "https://gw.example",
			raw:  "https://cdn.qwenlm.ai/output/u/t2i/c/a.png",
			want: "https://gw.example/v1/media/output/u/t2i/c/a.png",
		},
		{
			// 没配 blob_base_url 且推不出 Host 时宁可用原链：至少调用方知道自己在跟谁说话。
			name: "base 为空 ⇒ 原样返回",
			base: "",
			raw:  good,
			want: good,
		},
		{
			name: "空 URL",
			base: "https://gw.example",
			raw:  "",
			want: "",
		},
		{
			// 防 SSRF：绝不能把别的站塞进代理。
			name: "非上游 host ⇒ 原样返回",
			base: "https://gw.example",
			raw:  "https://evil.example/output/u/a.png?key=k",
			want: "https://evil.example/output/u/a.png?key=k",
		},
		{
			name: "http 不代理（只认 https）",
			base: "https://gw.example",
			raw:  "http://cdn.qwenlm.ai/output/u/a.png",
			want: "http://cdn.qwenlm.ai/output/u/a.png",
		},
		{
			name: "path 不在 /output/ 下 ⇒ 原样返回",
			base: "https://gw.example",
			raw:  "https://cdn.qwenlm.ai/etc/passwd",
			want: "https://cdn.qwenlm.ai/etc/passwd",
		},
		{
			name: "含 .. ⇒ 原样返回",
			base: "https://gw.example",
			raw:  "https://cdn.qwenlm.ai/output/../../secret",
			want: "https://cdn.qwenlm.ai/output/../../secret",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proxyImageURL(c.base, c.raw); got != c.want {
				t.Fatalf("proxyImageURL(%q, %q)\n got %q\nwant %q", c.base, c.raw, got, c.want)
			}
		})
	}
}

// TestProxyImageURLKeepsPathEscaping 上游 path 若带转义字符，改写后不能丢/重复转义。
func TestProxyImageURLKeepsPathEscaping(t *testing.T) {
	raw := "https://cdn.qwenlm.ai/output/u%20x/t2i/c/a.png?key=k"
	want := "https://gw.example/v1/media/output/u%20x/t2i/c/a.png?key=k"
	if got := proxyImageURL("https://gw.example", raw); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestPassThroughMediaSendsBrowserHeaders 核心回归：UA 与 Referer 必须转发。
func TestPassThroughMediaSendsBrowserHeaders(t *testing.T) {
	s := newProxyServer(t, nil)

	var mu sync.Mutex
	var gotUA, gotReferer, gotAccept string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotUA, gotReferer, gotAccept = r.Header.Get("User-Agent"), r.Header.Get("Referer"), r.Header.Get("Accept")
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(testPNG)))
		_, _ = w.Write([]byte(testPNG))
	}))
	defer up.Close()

	rec := httptest.NewRecorder()
	s.passThroughMedia(rec, httptest.NewRequest(http.MethodGet, "/v1/media/output/u/t2i/c/a.png?key=k", nil),
		up.URL+"/output/u/t2i/c/a.png?key=k")

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200（body=%q）", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != testPNG {
		t.Fatalf("字节没透传：%q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q，期望透传 image/png", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
		t.Fatalf("Cache-Control=%q，期望带 max-age", cc)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotUA != s.conf().Upstream.UserAgent {
		t.Fatalf("UA 没带对：%q", gotUA)
	}
	if gotUA == "" || strings.HasPrefix(gotUA, "Go-http-client") {
		t.Fatalf("UA 是空/Go 默认值（会被上游 WAF 判成 404）：%q", gotUA)
	}
	if gotReferer != mediaReferer {
		t.Fatalf("Referer 没带对：%q，期望 %q", gotReferer, mediaReferer)
	}
	if gotAccept == "" {
		t.Fatal("Accept 为空")
	}
}

// TestPassThroughMediaFallbackMIME 上游回 application/octet-stream 时要按扩展名修正，
// 否则浏览器会把图当附件下载（fetchMedia 里踩过同一个坑）。
func TestPassThroughMediaFallbackMIME(t *testing.T) {
	s := newProxyServer(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(testPNG))
	}))
	defer up.Close()

	rec := httptest.NewRecorder()
	s.passThroughMedia(rec, httptest.NewRequest(http.MethodGet, "/v1/media/output/u/a.png", nil),
		up.URL+"/output/u/a.png")
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q，期望按扩展名补成 image/png", ct)
	}
}

// TestPassThroughMediaForwardsRange 视频帧取图会带 Range，必须原样转给上游。
func TestPassThroughMediaForwardsRange(t *testing.T) {
	s := newProxyServer(t, nil)
	var gotRange string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(testPNG[:4]))
	}))
	defer up.Close()

	req := httptest.NewRequest(http.MethodGet, "/v1/media/output/u/a.png", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	s.passThroughMedia(rec, req, up.URL+"/output/u/a.png")

	if gotRange != "bytes=0-3" {
		t.Fatalf("Range 没转发：%q", gotRange)
	}
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("状态码 %d，期望 206", rec.Code)
	}
	if cr := rec.Header().Get("Content-Range"); cr == "" {
		t.Fatal("Content-Range 没透传")
	}
}

// TestPassThroughMediaNotFound 上游 404 ⇒ 网关 404（对 <img> 而言就是"没图"）。
func TestPassThroughMediaNotFound(t *testing.T) {
	s := newProxyServer(t, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer up.Close()

	rec := httptest.NewRecorder()
	s.passThroughMedia(rec, httptest.NewRequest(http.MethodGet, "/v1/media/output/u/a.png", nil),
		up.URL+"/output/u/a.png")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", rec.Code)
	}
}

// TestPassThroughMediaRetriesNotYetStored 刚出图那几秒上游会短暂 404，重试一次要能救回来。
func TestPassThroughMediaRetriesNotYetStored(t *testing.T) {
	s := newProxyServer(t, nil)
	var mu sync.Mutex
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			http.Error(w, "not yet", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(testPNG))
	}))
	defer up.Close()

	rec := httptest.NewRecorder()
	s.passThroughMedia(rec, httptest.NewRequest(http.MethodGet, "/v1/media/output/u/a.png", nil),
		up.URL+"/output/u/a.png")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，期望重试后 200（body=%q）", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("上游被调用 %d 次，期望 2（1 次 404 + 1 次成功）", calls)
	}
}

// TestImageProxyRejectsBadPath 非法路径直接 404，且**不能**构造出外发请求。
func TestImageProxyRejectsBadPath(t *testing.T) {
	for _, p := range []string{
		"/v1/media/",
		"/v1/media/etc/passwd",
		"/v1/media/output/../secret",
	} {
		s := newProxyServer(t, nil)
		rec := httptest.NewRecorder()
		// 直接调 handler：走 ServeMux 时 /../ 会先被 cleanPath 规范成 301，
		// 就测不到本函数里那道 ".." 检查了。
		s.handleImageProxy(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s 返回 %d，期望 404", p, rec.Code)
		}
	}
}

// TestImageProxyIsUnauthenticated 配了 API Key 也不能拦这个端点：
// 浏览器 <img> 不会带 Authorization，一拦整个功能就废了。
func TestImageProxyIsUnauthenticated(t *testing.T) {
	s := newProxyServer(t, func(c *config.Config) {
		c.APIKeys = []string{"sk-test"}
	})
	rec := httptest.NewRecorder()
	// 用非法 path 避免真的发外网请求；401 与 404 足以区分"被鉴权拦"与"路由通了"。
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/media/etc/passwd", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("图片代理被鉴权拦住了：<img> 不会带 API Key，必须免鉴权")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 %d，期望 404", rec.Code)
	}
}

// TestImagesGenerationReturnsUpstreamURL 端到端钉住"url 就是上游 cdn 原链"。
//
// 🔴 方向与 2026-11-02 早先版本相反：当时改成返回网关代理地址（因为原链有
// UA + Referer 防盗链、调用方拿到手会 403/404）。用户随后明确要求**全部给
// cdn.qwenlm.ai**，于是回退。这条测试现在守的是"别又被顺手改回代理地址"，
// 以及"网关域名绝不能渗进响应体"。
func TestImagesGenerationReturnsUpstreamURL(t *testing.T) {
	const upstreamURL = "https://cdn.qwenlm.ai/output/u1/t2i/c1/abc.png?key=k"
	up := newImageUpstream(t)
	up.productURL = upstreamURL

	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	cfg.ImageModel = "qwen-image-3.0"
	cfg.BlobBaseURL = "https://qwen2api.1task.cn"

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	h := New(cfg, p, cli, reqlog.New(10), "test").Handler()

	rec := postImage(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("生图应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp ImageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 🔴 2026-11-02 用户要求：`url` 必须是**上游 cdn 原链**，响应里不出现网关域名。
	if len(resp.Data) != 1 || resp.Data[0].URL != upstreamURL {
		t.Fatalf("应原样返回上游直链 %q，实际 %+v", upstreamURL, resp.Data)
	}
	// 反向断言：绝不能被改写成网关的 /v1/media 代理地址（哪怕 blob_base_url 配了）。
	if strings.Contains(resp.Data[0].URL, "/v1/media/") {
		t.Fatalf("url 被改写成了网关代理地址：%q", resp.Data[0].URL)
	}
}

// TestImageProxyMethodGuard 只允许 GET/HEAD。
func TestImageProxyMethodGuard(t *testing.T) {
	s := newProxyServer(t, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/media/output/u/a.png", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 返回 %d，期望 405", rec.Code)
	}
}
