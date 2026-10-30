package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- 图片输入（chat 全形态） ----------
//
// 契约：content 里的图片段一律翻成上游 files[]（file_class=vision，chat_type 仍 t2t），
// 引用形态三收 —— URL / data URI / 裸 base64，与 /v1/images 的 `image` 字段共用
// resolveImageRef 那条解链（image-adapter 的既定规范）。

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg=="

func imageRefsFor(t *testing.T, messages string) ([]string, error) {
	t.Helper()
	var msgs []ChatMessage
	if err := json.Unmarshal([]byte(messages), &msgs); err != nil {
		t.Fatalf("messages 解析失败: %v", err)
	}
	return collectImageRefs(msgs)
}

func TestImageRefForms(t *testing.T) {
	cases := []struct {
		name string
		part string
		want string
	}{
		{"image_url 对象（OpenAI 官方）",
			`{"type":"image_url","image_url":{"url":"https://cdn.example.com/a.png"}}`,
			"https://cdn.example.com/a.png"},
		{"image_url 字符串（部分 SDK）",
			`{"type":"image_url","image_url":"https://cdn.example.com/b.png"}`,
			"https://cdn.example.com/b.png"},
		{"image_url data URI",
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}`,
			"data:image/png;base64," + onePixelPNG},
		{"image_url 裸 base64",
			`{"type":"image_url","image_url":{"url":"` + onePixelPNG + `"}}`,
			onePixelPNG},
		{"input_image（Responses 形态）",
			`{"type":"input_image","image_url":"https://cdn.example.com/c.png"}`,
			"https://cdn.example.com/c.png"},
		{"Anthropic base64 源",
			`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}`,
			"data:image/jpeg;base64,QUJD"},
		{"Anthropic url 源",
			`{"type":"image","source":{"type":"url","url":"https://cdn.example.com/d.png"}}`,
			"https://cdn.example.com/d.png"},
		{"平铺裸 base64（少数 SDK）",
			`{"type":"image","data":"` + onePixelPNG + `"}`,
			onePixelPNG},
		{"image_url 里的 detail 被忽略",
			`{"type":"image_url","image_url":{"url":"https://x/e.png","detail":"high"}}`,
			"https://x/e.png"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refs, err := imageRefsFor(t, `[{"role":"user","content":[{"type":"text","text":"看图"},`+c.part+`]}]`)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if len(refs) != 1 || refs[0] != c.want {
				t.Fatalf("引用不对: %#v（期望 %q）", refs, c.want)
			}
		})
	}
}

func TestImageRefsKeepOrderAcrossMessages(t *testing.T) {
	refs, err := imageRefsFor(t, `[
		{"role":"user","content":[{"type":"image_url","image_url":"https://x/1.png"}]},
		{"role":"assistant","content":"看到红的了"},
		{"role":"user","content":[
			{"type":"text","text":"那这两张呢"},
			{"type":"image_url","image_url":"https://x/2.png"},
			{"type":"image","source":{"type":"url","url":"https://x/3.png"}}]}]`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"https://x/1.png", "https://x/2.png", "https://x/3.png"}
	if len(refs) != len(want) {
		t.Fatalf("引用数量 %d，期望 %d: %#v", len(refs), len(want), refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("第 %d 张引用 %q，期望 %q", i+1, refs[i], want[i])
		}
	}
}

// 🔴 坏引用必须 400：跳过这张图继续聊＝模型看不见图却照常作答。
func TestImageRefsRejectBrokenForms(t *testing.T) {
	cases := []struct {
		name string
		part string
		want string
	}{
		{"完全没有引用", `{"type":"image_url"}`, "缺少图片引用"},
		{"image_url 空串", `{"type":"image_url","image_url":""}`, "空字符串"},
		{"image_url.url 空", `{"type":"image_url","image_url":{"url":""}}`, "url 为空"},
		{"source.type 不认识", `{"type":"image","source":{"type":"file","file_id":"x"}}`, "不支持"},
		{"source.data 空", `{"type":"image","source":{"type":"base64","media_type":"image/png"}}`, "data 为空"},
		{"source.url 空", `{"type":"image","source":{"type":"url"}}`, "url 为空"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := imageRefsFor(t, `[{"role":"user","content":[`+c.part+`]}]`)
			if err == nil {
				t.Fatalf("应当报错（%s）", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误文案不含 %q: %v", c.want, err)
			}
			// 错误里要能定位到第几条消息 / 第几个 part。
			if !strings.Contains(err.Error(), "messages[0]") {
				t.Fatalf("错误缺定位信息: %v", err)
			}
		})
	}
}

func TestImageRefsIgnoreNonImageParts(t *testing.T) {
	// 文本/文件段不产生图片引用（file 走 collectFiles，不该被当成图）。
	refs, err := imageRefsFor(t, `[{"role":"user","content":[
		{"type":"text","text":"看这份文件"},
		{"type":"file","file":{"file_id":"file-abc"}},
		{"type":"image_url","image_url":"https://x/only.png"}]}]`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(refs) != 1 || refs[0] != "https://x/only.png" {
		t.Fatalf("引用不对: %#v", refs)
	}
}

// 端到端：URL 形态的图片**原样**进 files[]（上游自己去拉），不碰 getstsToken。
func TestChatImageURLPassthrough(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"text","text":"这张图什么颜色"},
		{"type":"image_url","image_url":"https://cdn.example.com/a.png"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(up.gotSTS) != 0 {
		t.Fatalf("URL 形态不该调 getstsToken（实测该端点被 WAF 挑战）: %+v", up.gotSTS)
	}
	files := submitFiles(t, up.gotSubmit)
	if len(files) != 1 {
		t.Fatalf("files 数量: %d", len(files))
	}
	if files[0]["url"] != "https://cdn.example.com/a.png" {
		t.Fatalf("url 应为原样外链: %+v", files[0])
	}
	if files[0]["file_class"] != "vision" || files[0]["showType"] != "image" {
		t.Fatalf("vision 标记不对: %+v", files[0])
	}
	// 图片不参与文本拍平（否则 prompt 里会多出一堆 URL）。
	msg := up.gotSubmit["messages"].([]any)[0].(map[string]any)
	if msg["content"] != "这张图什么颜色" {
		t.Fatalf("prompt 被图片污染: %q", msg["content"])
	}
}

// data URI / 裸 base64：落本地 blob，给上游一个自己能拉的 URL，并验证该 URL 真能取回字节。
func TestChatImageBase64GoesToBlob(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	files := submitFiles(t, up.gotSubmit)
	if len(files) != 1 {
		t.Fatalf("files 数量: %d", len(files))
	}
	rawURL, _ := files[0]["url"].(string)
	u, err := url.Parse(rawURL)
	if err != nil || !strings.HasPrefix(u.Path, "/v1/blobs/") {
		t.Fatalf("url 应指向本地 blob: %q", rawURL)
	}
	if !strings.HasSuffix(u.Path, ".png") {
		t.Fatalf("blob 路径应带 .png 后缀: %q", u.Path)
	}
	// blob 端点免鉴权可读（上游取图不带我们的 Key）。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, u.Path, nil))
	if rec2.Code != 200 {
		t.Fatalf("blob 取图失败: %d %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("Content-Type 不对: %q", rec2.Header().Get("Content-Type"))
	}
	if got := rec2.Body.Bytes(); len(got) == 0 || string(got[:4]) != "\x89PNG" {
		t.Fatalf("blob 内容不是 PNG（前 4 字节 %q，共 %d 字节）", string(got[:min(4, len(got))]), len(got))
	}
}

// 未知 blob id → 404（不能回空图让上游以为成功）。
func TestBlobUnknownIs404(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/blobs/deadbeef.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 blob 应 404，得到 %d", rec.Code)
	}
}

// submitFiles 取提交体里的 files[]（fake 上游记录的是 map）。
func submitFiles(t *testing.T, submit map[string]any) []map[string]any {
	t.Helper()
	if submit == nil {
		t.Fatal("上游未收到提交")
	}
	msgs, _ := submit["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("提交里没有 messages")
	}
	first, _ := msgs[0].(map[string]any)
	raw, _ := first["files"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, f := range raw {
		if m, ok := f.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestChatImageTooManyRejected(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	parts := make([]string, 0, maxInputImages+1)
	parts = append(parts, `{"type":"text","text":"一堆图"}`)
	for i := 0; i < maxInputImages+1; i++ {
		parts = append(parts, fmt.Sprintf(`{"type":"image_url","image_url":"https://x/%d.png"}`, i))
	}
	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[` +
		strings.Join(parts, ",") + `]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超过 %d 张应 400，得到 %d", maxInputImages, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"param":"image"`) {
		t.Fatalf("错误应指向 param=image: %s", rec.Body.String())
	}
	if len(up.gotSTS) != 0 || up.calls != 0 {
		t.Fatalf("超限请求不该触达上游（calls=%d sts=%d）", up.calls, len(up.gotSTS))
	}
}

// Anthropic / Responses 的图片块必须保留（不能因为转格式就丢掉）。
func TestAnthropicContentKeepsImage(t *testing.T) {
	raw, err := anthropicContentRaw(json.RawMessage(
		`[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("输出不是数组: %v (%s)", err, raw)
	}
	if len(parts) != 2 || parts[1]["type"] != "image" {
		t.Fatalf("图片块丢了: %s", raw)
	}
	// 再走一遍 chat 门的收集，确认能取到引用。
	refs, err := collectImageRefs([]ChatMessage{{Role: "user", Content: raw}})
	if err != nil || len(refs) != 1 || refs[0] != "data:image/png;base64,QUJD" {
		t.Fatalf("引用不对: %#v %v", refs, err)
	}
}

func TestAnthropicContentRejectsUnsupportedBlocks(t *testing.T) {
	if _, err := anthropicContentRaw(json.RawMessage(
		`[{"type":"tool_use","id":"t","name":"f","input":{}}]`)); err == nil {
		t.Fatal("tool_use 应 400")
	}
	if _, err := anthropicContentRaw(json.RawMessage(
		`[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}]`)); err == nil {
		t.Fatal("document 应 400")
	}
}

func TestMergeAttachmentsOrder(t *testing.T) {
	stored := []map[string]any{{"id": "file-stored"}}
	uploaded := []map[string]any{{"id": "file-img1"}, {"id": "file-img2"}}
	merged := mergeAttachments(stored, uploaded)
	if len(merged) != 3 {
		t.Fatalf("数量不对: %d", len(merged))
	}
	if merged[0]["id"] != "file-stored" || merged[2]["id"] != "file-img2" {
		t.Fatalf("顺序不对: %+v", merged)
	}
	// 只有图片时不复制切片（常见路径）。
	only := mergeAttachments(nil, uploaded)
	if len(only) != 2 || only[0]["id"] != "file-img1" {
		t.Fatalf("纯图片路径不对: %+v", only)
	}
}

// 图片走 url 直通/本地 blob 后，**完全不再碰凭据链**：
// 不调 getstsToken、不做上传前续期（那条 ensureFresh 现在只服务 image_edit）。
func TestChatImageDoesNotTouchUploadChain(t *testing.T) {
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.StateFile = ""
	// token 不是 JWT ⇒ 寿命未知；若还在走"上传前续期"，这里必然打 refresh。
	cfg.Accounts = []config.Account{{Name: "a0", Token: "stale-not-a-jwt"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	p.SetRefresh("a0", "rt-0")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	h := s.Handler()

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if len(up.gotSTS) != 0 {
		t.Fatalf("不该再调 getstsToken: %+v", up.gotSTS)
	}
	if up.gotRefresh != 0 {
		t.Fatalf("图片路径不该触发续期，实际 %d 次", up.gotRefresh)
	}
}

func TestRenewableErrClassifiesDisguisedWAF(t *testing.T) {
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "stale", Token: "not-jwt"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")
	stale := p.Get("stale")

	if !s.renewableErr(stale, upstream.NewErr(upstream.KindAuth, "x", 401, "", "e", true)) {
		t.Fatal("KindAuth 应可续期")
	}
	// 过期 token + 风控类错误 ⇒ 真因是凭据，值得续期。
	if !s.renewableErr(stale, upstream.NewErr(upstream.KindRisk, "getstsToken", 200, "", "waf", true)) {
		t.Fatal("过期凭据上的 WAF 报错应可续期")
	}
	// 新鲜 token + 风控 ⇒ 真风控，续期没用。
	p.SetToken("stale", "bearer:eyJhbGciOiJub25lIn0."+base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))+".")
	if s.renewableErr(stale, upstream.NewErr(upstream.KindRisk, "getstsToken", 200, "", "waf", true)) {
		t.Fatal("新鲜凭据上的风控不该续期")
	}
}
