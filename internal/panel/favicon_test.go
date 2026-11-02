package panel

// 面板图标的测试。
//
// 守两件事：
//  1. 未登录（登录框旁边）也要有图标 —— 图标是纯静态资源，不能被鉴权挡成 401；
//  2. head 里确实声明了它 —— 只有路由没有 <link> 等于没加（浏览器不会主动请求）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestFaviconServedWithoutAuth(t *testing.T) {
	p, _ := withPanel(func(c *config.Config) { keyOnly(c) })

	rec := getWith(p, "/panel/favicon.svg", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("未登录也应能取图标，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("Content-Type 应为 image/svg+xml，得到 %q", ct)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, "<svg") || !strings.Contains(body, "viewBox=\"0 0 64 64\"") {
		t.Fatalf("图标内容不像 SVG: %.80s", body)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Fatalf("图标应可缓存，得到 Cache-Control=%q", cc)
	}
}

func TestIndexLinksFavicon(t *testing.T) {
	p, _ := withPanel(nil)

	rec := getWith(p, "/panel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("面板页应 200，得到 %d", rec.Code)
	}
	html := rec.Body.String()
	if !strings.Contains(html, `<link rel="icon" type="image/svg+xml" href="/panel/favicon.svg">`) {
		t.Fatal("index.html 的 head 缺少 favicon <link>")
	}
}
