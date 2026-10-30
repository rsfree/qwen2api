package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// ---------- 断流：先交付尾帧，再认失败 ----------

// scriptedBody 按脚本吐字节，最后返回固定错误（模拟 chunked 流被掐断）。
type scriptedBody struct {
	chunks [][]byte
	err    error
	i      int
}

func (b *scriptedBody) Read(p []byte) (int, error) {
	if b.i < len(b.chunks) {
		n := copy(p, b.chunks[b.i])
		b.i++
		return n, nil
	}
	return 0, b.err
}

func (b *scriptedBody) Close() error { return nil }

func newScriptedStream(chunks [][]byte, err error) *Stream {
	return &Stream{
		body:   &scriptedBody{chunks: chunks, err: err},
		frames: &SSEFrames{},
		parser: NewParser(),
		buf:    make([]byte, 16<<10),
	}
}

// 尾帧没有以空行收尾（断流时最常见）也必须被解析出来 —— 那往往正是产物帧。
// 顺序：先给帧，下一步才报"意外断流"，这样上层能先收产物再认失败。
func TestStreamDeliversTailFrameBeforeUnexpectedEOF(t *testing.T) {
	full := []byte(`data: {"choices":[{"delta":{"content":"https://cdn.example/x.png","phase":"image_gen","status":"typing"}}]}` + "\n\n")
	tail := []byte(`data: {"choices":[{"delta":{"content":"https://cdn.example/y.png","phase":"image_gen","status":"typing"}}]}`)

	st := newScriptedStream([][]byte{full, tail}, io.ErrUnexpectedEOF)

	ev, ok, err := st.Next()
	if err != nil || !ok {
		t.Fatalf("首帧应正常返回: ok=%v err=%v", ok, err)
	}
	if len(ev.Images) != 1 || ev.Images[0] != "https://cdn.example/x.png" {
		t.Fatalf("首帧产物取错: %+v", ev.Images)
	}

	ev, ok, err = st.Next()
	if err != nil || !ok {
		t.Fatalf("尾帧必须先于错误交付: ok=%v err=%v", ok, err)
	}
	if len(ev.Images) != 1 || ev.Images[0] != "https://cdn.example/y.png" {
		t.Fatalf("尾帧产物被丢了: %+v", ev.Images)
	}

	if _, _, err = st.Next(); err == nil {
		t.Fatal("尾帧消费完后必须把「意外断流」报出来（不能当正常结束）")
	} else if KindOf(err) != KindTransport {
		t.Fatalf("断流应归为 KindTransport，得到 %v", KindOf(err))
	}
}

// 正常 EOF（对端优雅收尾）仍然是正常结束，不能报错 —— 否则每条流都会变成失败。
func TestStreamCleanEOFFinishesSilently(t *testing.T) {
	full := []byte(`data: {"choices":[{"delta":{"content":"https://cdn.example/x.png","phase":"image_gen","status":"typing"}}]}` + "\n\n")
	st := newScriptedStream([][]byte{full}, io.EOF)

	if _, ok, err := st.Next(); err != nil || !ok {
		t.Fatalf("首帧应正常: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.Next(); err != nil || ok {
		t.Fatalf("优雅 EOF 应当是 (ok=false, err=nil)，得到 ok=%v err=%v", ok, err)
	}
}

// ---------- 会话历史补捞 ----------

func newHistoryClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	return New(cfg.Upstream)
}

// 图片在 content_list（历史形态），且只取"最新一条带图回复"。
func TestLatestChatImagesPrefersNewestMessage(t *testing.T) {
	var gotPath, gotReferer string
	cli := newHistoryClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"chat":{"history":{"messages":{
			"m1":{"role":"assistant","timestamp":100,"content_list":[
				{"content":"https://cdn.example/old.png?key=k1","phase":"image_gen","status":"finished"}]},
			"m2":{"role":"assistant","timestamp":200,"content_list":[
				{"content":"这是模型说的一句话，不是图","phase":"answer","status":"finished"},
				{"content":"https://cdn.example/new.png?key=k2","phase":"image_gen","status":"finished"}]},
			"m3":{"role":"user","timestamp":300,"content_list":[]}
		}}}}}`))
	})

	got, err := cli.LatestChatImages(context.Background(), "tok", "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "https://cdn.example/new.png?key=k2" {
		t.Fatalf("应只返回最新一条带图回复，得到 %+v", got)
	}
	if !strings.Contains(gotPath, "/api/v2/chats/chat-1") || !strings.Contains(gotPath, "direction=up") {
		t.Fatalf("请求路径不对: %s", gotPath)
	}
	if !strings.HasSuffix(gotReferer, "/c/chat-1") {
		t.Fatalf("referer 必须是会话页，得到 %q", gotReferer)
	}
}

// SSE 那套 extra.image_list 也要收（两种形态并存，只认一种会漏）。
func TestLatestChatImagesAcceptsExtraImageList(t *testing.T) {
	cli := newHistoryClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"chat":{"history":{"messages":{
			"m1":{"role":"assistant","timestamp":1,"extra":{"image_list":[{"image":"https://cdn.example/a.png?key=k"}]}}
		}}}}}`))
	})
	got, err := cli.LatestChatImages(context.Background(), "tok", "chat-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "https://cdn.example/a.png?key=k" {
		t.Fatalf("extra.image_list 未被回收: %+v", got)
	}
}

// 历史里一条图都没有时必须返回空（而不是报错）——
// 上层据此判断"补捞无果，仍按原错误返回"。
func TestLatestChatImagesEmptyWhenNoImages(t *testing.T) {
	cli := newHistoryClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"chat":{"history":{"messages":{
			"m1":{"role":"assistant","timestamp":1,"error":{"code":"internal_error"},
			        "content_list":[]}
		}}}}}`))
	})
	got, err := cli.LatestChatImages(context.Background(), "tok", "chat-1")
	if err != nil {
		t.Fatalf("无图不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不该凭空造出图片: %+v", got)
	}
}

// 上游把业务失败塞进 success:false 时，必须当成错误（否则会被读成"没图"，
// 上层就会把"权限/风控问题"误判成"模型没画"）。
func TestLatestChatImagesSurfacesBusinessError(t *testing.T) {
	cli := newHistoryClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"您没有权限访问此资源"}}`))
	})
	_, err := cli.LatestChatImages(context.Background(), "tok", "chat-1")
	if err == nil {
		t.Fatal("业务失败必须报错")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型应是 *upstream.Error，得到 %T", err)
	}
	if KindOf(err) != KindAuth {
		t.Fatalf("应归为 KindAuth，得到 %v", KindOf(err))
	}
}

// JSON 形状漂移时给出可诊断的错误，而不是静默返回空。
func TestLatestChatImagesRejectsUnexpectedShape(t *testing.T) {
	cli := newHistoryClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"chat":"not-an-object"}}`))
	})
	if _, err := cli.LatestChatImages(context.Background(), "tok", "chat-1"); err == nil {
		t.Fatal("形态不符必须报错")
	}
}
