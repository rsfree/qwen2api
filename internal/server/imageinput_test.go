package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// 端到端：图片请求必须真的走 STS 上传链，而不是"静默丢图去聊"。
// 假上游在 getstsToken 处返回 500 —— 断言错误落在上传这一步，且 completions 未被调用。
func TestChatImageGoesThroughUploadChain(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"text","text":"这张图是什么颜色？"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + onePixelPNG + `"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("图片不该再被当成不支持: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("上传失败应 502，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "上传失败") {
		t.Fatalf("错误应指明上传环节: %s", rec.Body.String())
	}
	// 上传确实发生了，且按图片类型申请（filetype=image 决定上游的 vision 通道）。
	if len(up.gotSTS) != 1 {
		t.Fatalf("getstsToken 应调用 1 次，实际 %d 次", len(up.gotSTS))
	}
	if up.gotSTS[0]["filetype"] != "image" {
		t.Fatalf("filetype 应为 image: %+v", up.gotSTS[0])
	}
	if name, _ := up.gotSTS[0]["filename"].(string); !strings.HasSuffix(name, ".png") {
		t.Fatalf("文件名应带 .png 后缀（魔数嗅探结果）: %+v", up.gotSTS[0])
	}
	// 上传失败就绝不能提交对话（否则模型看不到图还照常作答）。
	if up.gotSubmit != nil {
		t.Fatalf("上传失败后仍提交了对话: %+v", up.gotSubmit)
	}
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

// 🔴 过期凭据下上传图片：必须先续期（refresh_token），再用新 token 上传。
//
// 2026-10-30 实测的坑：getstsToken 对过期 Bearer 返回 200 + WAF 挑战页（不是 401），
// 所以"401 才续期"的兜底在这条路径上完全失效，还会把好号误冷却。
func TestChatImageRenewsStaleTokenBeforeUpload(t *testing.T) {
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL // refresh 也落到假上游
	cfg.StateFile = ""
	// token 不是 JWT ⇒ TokenExpiry 未知 ⇒ 视为不新鲜（正是生产里的常态）。
	cfg.Accounts = []config.Account{{Name: "a0", Token: "stale-not-a-jwt"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	p.SetRefresh("a0", "rt-0")
	cli := upstream.New(cfg.Upstream)
	s := New(cfg, p, cli, reqlog.New(10), "test")
	h := s.Handler()

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if up.gotRefresh == 0 {
		t.Fatalf("上传前应先续期，refresh 未被调用（状态 %d）: %s", rec.Code, rec.Body.String())
	}
	if len(up.stsAuth) == 0 {
		t.Fatalf("getstsToken 未被调用: %s", rec.Body.String())
	}
	if up.stsAuth[0] != "Bearer fresh-access" {
		t.Fatalf("上传用的不是续期后的新 token: %q", up.stsAuth[0])
	}
}

// token 新鲜时不该白白多打一次 refresh。
func TestChatImageDoesNotRefreshFreshToken(t *testing.T) {
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = nil
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	// 造一个 1 小时后才过期的合法 JWT（只需要 payload.exp 可解析）。
	exp := time.Now().Add(time.Hour).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		fmt.Sprintf(`{"exp":%d}`, exp)))
	jwt := "eyJhbGciOiJub25lIn0." + payload + "."
	p.Add(config.Account{Name: "a0", Token: "bearer:" + jwt})
	p.SetRefresh("a0", "rt-0")
	cli := upstream.New(cfg.Upstream)
	s := New(cfg, p, cli, reqlog.New(10), "test")
	h := s.Handler()

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":[
		{"type":"image_url","image_url":"data:image/png;base64,` + onePixelPNG + `"}]}]}`
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if up.gotRefresh != 0 {
		t.Fatalf("token 还新鲜却续期了 %d 次", up.gotRefresh)
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
