package upstream

import (
	"strings"
	"testing"
	"time"
)

// mustTime 固定时刻（2026-10-28 17:48:27 +08:00 = 抓包那条请求的时间）。
func mustTime() time.Time {
	return time.Date(2026, 10, 28, 17, 48, 27, 0, time.FixedZone("CST", 8*3600))
}

// 真实帧（2026-10-28 一手抓包，去掉了凭据相关字段）——
// 测试的价值全在这里：形态不是编的。
const (
	frameCreated  = `{"response.created":{"chat_id":"11111111-1111-4111-8111-111111111111","parent_id":"22222222-2222-4222-8222-222222222222","response_id":"33333333-3333-4333-8333-333333333333","response_index":"0"}}`
	frameThink    = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","extra":{"summary_title":{"content":["解析用户指令并执行计算"]},"summary_thought":{"content":["我首先审视了用户的提问。","随后我完成了基础运算。"]}},"status":"typing"}}],"response_id":"a0ed796f","usage":{"input_tokens":1239,"output_tokens":105,"characters":0,"total_tokens":1344,"input_tokens_details":{"text_tokens":1239},"output_tokens_details":{"reasoning_tokens":99,"text_tokens":105}},"timestamp":1793181438}`
	frameThinkFin = `{"choices":[{"delta":{"role":"assistant","content":"","phase":"thinking_summary","status":"finished"}}],"response_id":"a0ed796f","timestamp":1793181438}`
	frameAnswer   = `{"choices":[{"delta":{"role":"assistant","content":"2","phase":"answer","status":"typing"}}],"response_id":"a0ed796f","usage":{"input_tokens":1239,"output_tokens":105,"total_tokens":1344},"timestamp":1793181438}`
	frameFinish   = `{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"answer"}}],"response_id":"a0ed796f"}`
)

func TestParserFullSequence(t *testing.T) {
	p := NewParser()

	ev, ok, err := p.Parse([]byte(frameCreated))
	if err != nil || !ok || ev.Created == nil {
		t.Fatalf("created 帧解析失败: ok=%v err=%v ev=%+v", ok, err, ev)
	}
	if ev.Created.ChatID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("chat_id 取错: %q", ev.Created.ChatID)
	}
	if ev.Created.ResponseID != "33333333-3333-4333-8333-333333333333" {
		t.Fatalf("response_id 取错: %q", ev.Created.ResponseID)
	}

	ev, _, _ = p.Parse([]byte(frameThink))
	if !strings.Contains(ev.Reasoning, "【解析用户指令并执行计算】") {
		t.Fatalf("思考标题未提取: %q", ev.Reasoning)
	}
	if !strings.Contains(ev.Reasoning, "我首先审视了用户的提问。") {
		t.Fatalf("思考正文未提取: %q", ev.Reasoning)
	}
	// 思考帧 content 恒空 ⇒ 不得当成正文。
	if ev.Text != "" {
		t.Fatalf("thinking 帧不该产出正文: %q", ev.Text)
	}
	if ev.Usage == nil || ev.Usage.InputTokens != 1239 || ev.Usage.OutputTokensDetails.ReasoningTokens != 99 {
		t.Fatalf("usage 未解析: %+v", ev.Usage)
	}

	// 累加数组必须 diff：同一份 extra 再发一次，不能重复吐出。
	ev2, _, _ := p.Parse([]byte(frameThink))
	if ev2.Reasoning != "" {
		t.Fatalf("重复思考帧应 diff 成空，得到 %q", ev2.Reasoning)
	}

	ev, _, _ = p.Parse([]byte(frameThinkFin))
	if ev.Done {
		t.Fatal("thinking_summary 的 finished 不能当成整条流结束（那会把思考阶段就掐断）")
	}

	ev, _, _ = p.Parse([]byte(frameAnswer))
	if ev.Text != "2" {
		t.Fatalf("正文增量取错: %q", ev.Text)
	}
	if ev.Done {
		t.Fatal("typing 帧不该结束")
	}

	ev, _, _ = p.Parse([]byte(frameFinish))
	if !ev.Done {
		t.Fatal("finished + phase=answer 必须判定为结束")
	}
}

func TestParserThinkingFinishedIsNotDone(t *testing.T) {
	// 单独再钉一次：这两帧只差 phase 一个键，判宽了会把流截断在思考阶段。
	p := NewParser()
	if ev, _, _ := p.Parse([]byte(frameThinkFin)); ev.Done {
		t.Fatal("phase=thinking_summary 的 finished 不是结束")
	}
	if ev, _, _ := p.Parse([]byte(frameFinish)); !ev.Done {
		t.Fatal("phase=answer 的 finished 是结束")
	}
}

func TestParserStreamError(t *testing.T) {
	p := NewParser()
	for _, payload := range []string{
		`{"error":"Internal error!"}`,
		`{"error":{"code":"invalid_input","details":"bad file"}}`,
	} {
		// 流内错误 = HTTP 200 包着错误：必须返回 error，不能静默当"这帧没内容"。
		if _, _, err := p.Parse([]byte(payload)); err == nil {
			t.Fatalf("流内错误必须响亮失败: payload=%s", payload)
		}
	}
}

func TestParserIgnoresHeartbeat(t *testing.T) {
	p := NewParser()
	for _, payload := range []string{"", "   ", "[DONE]", ": keep-alive", "not json"} {
		if _, ok, err := p.Parse([]byte(payload)); ok || err != nil {
			t.Fatalf("心跳/结束符应被忽略: %q ok=%v err=%v", payload, ok, err)
		}
	}
}

func TestSSEFramesSplit(t *testing.T) {
	var f SSEFrames
	// 一个帧被拆成三段推送（真实网络就是这样）。
	whole := "data: " + frameAnswer + "\n\ndata: " + frameFinish + "\n\n"
	cut1, cut2 := 20, 90
	batch := f.Push([]byte(whole[:cut1]))
	if len(batch) != 0 {
		t.Fatalf("不完整帧不该产出: %d", len(batch))
	}
	batch = append(batch, f.Push([]byte(whole[cut1:cut2]))...)
	batch = append(batch, f.Push([]byte(whole[cut2:]))...)
	if len(batch) != 2 {
		t.Fatalf("应切出 2 帧，得到 %d", len(batch))
	}
	if !strings.Contains(string(batch[0]), `"content":"2"`) {
		t.Fatalf("第一帧内容不对: %s", batch[0])
	}
}

func TestSSEFramesCRLFAndMultiline(t *testing.T) {
	var f SSEFrames
	batch := f.Push([]byte("data: {\"a\":1}\r\n\r\nevent: x\r\ndata: {\"b\":\r\ndata: 2}\r\n\r\n"))
	if len(batch) != 2 {
		t.Fatalf("CRLF 分帧失败: %d", len(batch))
	}
	if string(batch[1]) != "{\"b\":\n2}" {
		t.Fatalf("多行 data 拼接错误: %q", batch[1])
	}
}

func TestBuildSubmitShape(t *testing.T) {
	body := BuildSubmit("chat-1", "", "qwen3.7-plus", "hi", "auto", mustTime())
	if !body.Stream || body.Version != "2.1" || !body.IncrementalOutput {
		t.Fatalf("顶层流式字段不对: %+v", body)
	}
	// 双写必须都在（缺小写会 400）。
	if body.ChatID != "chat-1" || body.ChatIDSnake != "chat-1" {
		t.Fatalf("chatId 必须双写: %+v", body)
	}
	if body.ParentID != "" || body.ParentIDSnake != nil {
		t.Fatalf("新会话 parentId 应为空串、parent_id 应为 null: %q %v", body.ParentID, body.ParentIDSnake)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("messages 长度: %d", len(body.Messages))
	}
	m := body.Messages[0]
	if m.ID != nil {
		t.Fatal("messages[0].id 必须是 null")
	}
	if m.Files == nil {
		t.Fatal("files 必须是 []，不能是 null")
	}
	if m.ChatType != "t2t" || m.SubChatType != "t2t" {
		t.Fatalf("chat_type 标记不对: %q %q", m.ChatType, m.SubChatType)
	}
	if !m.FeatureConfig.ThinkingEnabled || m.FeatureConfig.ThinkingMode != "Auto" {
		t.Fatalf("auto 档配置不对: %+v", m.FeatureConfig)
	}
}

func TestBuildSubmitParentChain(t *testing.T) {
	body := BuildSubmit("chat-1", "msg-prev", "qwen3.7-plus", "hi", "thinking", mustTime())
	if body.ParentID != "msg-prev" {
		t.Fatalf("顶层 parentId 未带: %q", body.ParentID)
	}
	if body.Messages[0].ParentID == nil || *body.Messages[0].ParentID != "msg-prev" {
		t.Fatalf("messages[0].parentId 未带: %v", body.Messages[0].ParentID)
	}
	if body.Messages[0].FeatureConfig.AutoThinking {
		t.Fatal("thinking 档 auto_thinking 必须是 false")
	}
}

func TestGearFast(t *testing.T) {
	c := GearConfig("fast")
	if c.ThinkingEnabled {
		t.Fatal("fast 档必须关思考")
	}
	if c.ThinkingMode != "Fast" {
		t.Fatalf("fast 档 thinking_mode 应为 Fast: %q", c.ThinkingMode)
	}
	if c.ThinkingFormat != "" {
		t.Fatal("fast 档不该带 thinking_format")
	}
}

func TestTimezoneHeader(t *testing.T) {
	got := TimezoneHeader(mustTime())
	if !strings.HasPrefix(got, "Wed Oct 28 2026 17:48:27 GMT+0800") {
		t.Fatalf("timezone 头格式不符（必须是 GMT+0800 而不是 CST）: %q", got)
	}
}

func TestIsCookieJar(t *testing.T) {
	cases := map[string]bool{
		"eyJhbGciOi.xxx.yyy": false,
		"Bearer eyJhbGci":    false,
		"token=abc; other=1": true,
		"a=1; b=2":           true,
	}
	for in, want := range cases {
		if got := IsCookieJar(in); got != want {
			t.Fatalf("IsCookieJar(%q)=%v, want %v", in, got, want)
		}
	}
}
