package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func TestImageRefsForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
		err  bool
	}{
		{"未传", ``, 0, false},
		{"null", `null`, 0, false},
		{"空数组", `[]`, 0, false},
		{"单字符串", `"https://x/a.png"`, 1, false},
		{"数组", `["https://x/a.png","data:image/png;base64,AAA"]`, 2, false},
		{"空字符串", `""`, 0, true},
		{"数组含空串", `["a",""]`, 0, true},
		{"非法类型", `{"url":"x"}`, 0, true},
	}
	for _, c := range cases {
		var req ImageRequest
		if c.raw != "" {
			if err := json.Unmarshal([]byte(`{"image":`+c.raw+`}`), &req); err != nil {
				t.Fatalf("%s: 反序列化失败: %v", c.name, err)
			}
		}
		got, err := req.imageRefs()
		if c.err {
			if err == nil {
				t.Fatalf("%s: 应报错", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: 不该报错: %v", c.name, err)
		}
		if len(got) != c.want {
			t.Fatalf("%s: 得到 %d 个引用，want %d", c.name, len(got), c.want)
		}
	}
}

func TestSizeToRatio(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"1024x1024": "1:1",
		"1792x1024": "16:9",
		"1024x1792": "9:16",
		"1:1":       "1:1",
		"16:9":      "16:9",
		"auto":      "auto",
		"640x480":   "4:3",
		"480x640":   "3:4",
		"1920x1080": "16:9",
		"1000x1000": "1:1",
		"乱写":        "",
	}
	for in, want := range cases {
		if got := sizeToRatio(in); got != want {
			t.Fatalf("sizeToRatio(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestBuildImagePromptIsVerbatim(t *testing.T) {
	// 实测订正：prompt 必须原样发，加任何包装都会让 t2i 空转。
	in := "a cat wearing a beret"
	if got := buildImagePrompt(in, "16:9"); got != in {
		t.Fatalf("prompt 被改写了: %q", got)
	}
}

// 图生图已是真实链路；这里钉住"张数超限要明确 400 且不打上游"。
func TestImagesTooManyRefs(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	refs := make([]string, maxInputImages+1)
	for i := range refs {
		refs[i] = "https://x/a.png"
	}
	raw, _ := json.Marshal(map[string]any{"prompt": "x", "image": refs})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超限应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"param":"image"`) {
		t.Fatalf("错误应指明 param=image: %s", rec.Body.String())
	}
	if up.calls != 0 {
		t.Fatalf("参数不合法时不该打上游，实际 %d 次", up.calls)
	}
}

func TestImagesMaskWithoutImage(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"prompt":"x","mask":"https://x/m.png"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mask 无 image 应 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"param":"mask"`) {
		t.Fatalf("应指明 param=mask: %s", rec.Body.String())
	}
}

func TestImagesPromptRequired(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 prompt 应 400，得到 %d", rec.Code)
	}
}

func TestImagesResponseFormatNormalized(t *testing.T) {
	// 不认识的 response_format 视为"未指定"（默认 url），不报错。
	// 这里只验证它不会 400 —— 真正出图依赖上游。
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"prompt":"a cat","response_format":"weird"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("response_format 不该导致 400: %s", rec.Body.String())
	}
}

// edits 已是真实前门（multipart → 上传 → image_edit）。
// 这条钉住"不是 multipart 就明确 400"，而不是把它当 JSON 硬解。
func TestImagesEditsRejectsNonMultipart(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits",
		strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("/v1/images/edits 非 multipart 应 400，得到 %d", rec.Code)
	}
	if up.calls != 0 {
		t.Fatalf("参数不合法时不该打上游，实际 %d 次", up.calls)
	}
}

func TestVideoUnknownIDIs404(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/videos/video_nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知视频 id 应 404，得到 %d", rec.Code)
	}
}

func TestTranslateVideoStatus(t *testing.T) {
	task := &videoTask{taskID: "tsk_abc", model: "qwen3.7-plus"}

	cases := []struct {
		raw  map[string]any
		want string
	}{
		{map[string]any{"task_status": "SUCCESS"}, "completed"},
		{map[string]any{"task_status": "success"}, "completed"},
		{map[string]any{"task_status": "FAILED"}, "failed"},
		{map[string]any{"task_status": "RUNNING"}, "in_progress"},
		{map[string]any{}, "in_progress"},
	}
	for _, c := range cases {
		if got := translateVideo(task, c.raw).Status; got != c.want {
			t.Fatalf("task_status=%v → %s, want %s", c.raw, got, c.want)
		}
	}
}

func TestTranslateVideoExtractsURL(t *testing.T) {
	task := &videoTask{taskID: "tsk_abc"}
	// content 是 rsfree 实测的产物字段。
	got := translateVideo(task, map[string]any{
		"task_status": "SUCCESS",
		"content":     "https://cdn.example/v.mp4",
	})
	if got.URL != "https://cdn.example/v.mp4" {
		t.Fatalf("产物 URL 未提取: %+v", got)
	}
	// 没有产物时不能编一个。
	if v := translateVideo(task, map[string]any{"task_status": "SUCCESS"}); v.URL != "" {
		t.Fatalf("不该编造 URL: %q", v.URL)
	}
}

func TestResolveImageRefForms(t *testing.T) {
	// 1x1 PNG
	const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

	data, mime, _, err := resolveImageRef(context.Background(), "data:image/png;base64,"+tinyPNG)
	if err != nil {
		t.Fatalf("data URI 解析失败: %v", err)
	}
	if mime != "image/png" || len(data) == 0 {
		t.Fatalf("data URI 结果不对: mime=%q len=%d", mime, len(data))
	}

	// 裸 base64：mime 必须靠嗅探（SDK 常把 PNG 声明成 octet-stream）
	_, mime, _, err = resolveImageRef(context.Background(), tinyPNG)
	if err != nil {
		t.Fatalf("裸 base64 解析失败: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("嗅探出的 mime 不对: %q", mime)
	}

	if _, _, _, err := resolveImageRef(context.Background(), "not-a-ref"); err == nil {
		t.Fatal("非法引用应报错")
	}
	if _, _, _, err := resolveImageRef(context.Background(), ""); err == nil {
		t.Fatal("空引用应报错")
	}
}

func TestSniffMime(t *testing.T) {
	zeros := make([]byte, 12)
	png := append([]byte{0x89, 'P', 'N', 'G'}, zeros[4:]...)
	if got := sniffMime(png); got != "image/png" {
		t.Fatalf("PNG 嗅探失败: %q", got)
	}
	jpg := append([]byte{0xFF, 0xD8}, zeros[2:]...)
	if got := sniffMime(jpg); got != "image/jpeg" {
		t.Fatalf("JPEG 嗅探失败: %q", got)
	}
	if got := sniffMime([]byte("short")); got != "" {
		t.Fatalf("短数据不该识别: %q", got)
	}
	mp4 := []byte{0, 0, 0, 0, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	if !looksLikeMedia(mp4) {
		t.Fatal("MP4 魔数识别失败")
	}
	// 典型死链：226 字节 HTML
	if looksLikeMedia([]byte("<!DOCTYPE html><html>")) {
		t.Fatal("HTML 不该被当媒体")
	}
}

func TestCollectFilesResolvesStoredEntry(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)

	// 直接往 store 里塞一条（模拟 /v1/files 上传后的状态）。
	s.files.put("file-abc", &storedFile{
		account: "a0", entry: upstream.FileRef{"id": "up-1"},
		filename: "a.txt", mime: "text/plain", bytes: 10, createdAt: time.Now(),
	})

	msgs := []ChatMessage{
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"读一下"},
			{"type":"file","file":{"file_id":"file-abc"}}]`)},
	}
	files, _, err := s.collectFiles(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0]["id"] != "up-1" {
		t.Fatalf("file part 未解析成上游条目: %+v", files)
	}

	// 未知 id 必须报错（而不是静默当成"没附件"）。
	if _, _, err := s.collectFiles([]ChatMessage{{Role: "user",
		Content: json.RawMessage(`[{"type":"file","file":{"file_id":"nope"}}]`)}}); err == nil {
		t.Fatal("未知 file_id 应报错")
	}
}

func TestFilePartsDoNotBecomeText(t *testing.T) {
	// file part 不能混进正文（它是附件，不是文本）。
	msg := ChatMessage{Role: "user", Content: json.RawMessage(`[
		{"type":"text","text":"看这个"},
		{"type":"file","file":{"file_id":"file-x"}}]`)}
	text, err := msg.Text()
	if err != nil {
		t.Fatal(err)
	}
	if text != "看这个" {
		t.Fatalf("file part 污染了正文: %q", text)
	}
}

func TestFilesUploadRejectsEmpty(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	// 非 multipart
	req := httptest.NewRequest(http.MethodPost, "/v1/files", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非 multipart 应 400，得到 %d", rec.Code)
	}
	if up.calls != 0 {
		t.Fatalf("参数不合法不该打上游，实际 %d", up.calls)
	}
}

func TestFileGetUnknownIs404(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/files/file-nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 file id 应 404，得到 %d", rec.Code)
	}
}

// TestMaskWithoutImage 钉住 mask 的**有图/无图**分野（2026-10-30 实测发现的契约 bug：
// 老实现无条件 400，把 image+mask 的合法调用也打回，文案却写着"mask 没有 image"）。
//
// 语义：没有图 ⇒ 400（无从编辑）；有图 ⇒ 收下但不转发（上游无 inpaint，绝不假装按 mask 重绘）。
func TestMaskWithoutImage(t *testing.T) {
	cases := []struct {
		name string
		mask string
		refs []string
		want bool
	}{
		{"无 mask 无图", "", nil, false},
		{"只有 mask", "https://x/m.png", nil, true},
		{"mask 是空白", "   ", nil, false},
		{"mask + 一张图", "https://x/m.png", []string{"https://x/a.png"}, false},
		{"mask + 两张图", "https://x/m.png", []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := maskWithoutImage(c.mask, c.refs); got != c.want {
			t.Fatalf("%s: maskWithoutImage=%v, want %v", c.name, got, c.want)
		}
	}
}
