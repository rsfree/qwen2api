package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 对话键 ----------

func TestDialogKeyPriority(t *testing.T) {
	// 锚点必须够长（dialogMinAnchor），否则 hash 会撞 —— 这里用一句真实的任务描述。
	long := `"帮我总结这份季度财报里的三个主要风险点，并给出应对建议"`
	msgs := []ChatMessage{{Role: "user", Content: json.RawMessage(long)}}

	// 1. 显式头最高
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(dialogHeader, "abc")
	if k, src := dialogKeyOf(r, "u1", "m", msgs, true); src != "header" || k != "h:abc" {
		t.Fatalf("头优先失败: %q %q", k, src)
	}

	// 2. user 次之
	r.Header.Del(dialogHeader)
	if k, src := dialogKeyOf(r, "u1", "m", msgs, true); src != "user" || k != "u:u1" {
		t.Fatalf("user 次优失败: %q %q", k, src)
	}

	// 3. 都没有 → 首条用户消息的 hash
	k1, src := dialogKeyOf(r, "", "m", msgs, true)
	if src != "first_message" || !strings.HasPrefix(k1, "f:") {
		t.Fatalf("hash 兜底失败: %q %q", k1, src)
	}
	// 同一段对话的第二轮：messages 变长了，但首条没变 ⇒ 键必须一样
	msgs2 := append(msgs,
		ChatMessage{Role: "assistant", Content: json.RawMessage(`"好"`)},
		ChatMessage{Role: "user", Content: json.RawMessage(`"继续"`)})
	if k2, _ := dialogKeyOf(r, "", "m", msgs2, true); k2 != k1 {
		t.Fatalf("首条不变但键变了: %q vs %q", k1, k2)
	}
	// 换模型 ⇒ 换键（上游会话的 models[] 建的时候就定死了）
	if k3, _ := dialogKeyOf(r, "", "other-model", msgs, true); k3 == k1 {
		t.Fatal("换模型应换键")
	}
	// 没有 user 消息 ⇒ 不做粘性
	empty := []ChatMessage{{Role: "system", Content: json.RawMessage(`"sys"`)}}
	if k, src := dialogKeyOf(r, "", "m", empty, true); k != "" || src != "" {
		t.Fatalf("无用户消息应返回空键: %q %q", k, src)
	}
}

// 短首句不能当锚点：真实流量里"你好/1/继续"高度重复，撞键会串对话。
func TestDialogKeyRejectsShortAnchor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for _, short := range []string{"你好", "1", "继续", "在吗", "ok"} {
		msgs := []ChatMessage{{Role: "user", Content: json.RawMessage(`"` + short + `"`)}}
		if k, src := dialogKeyOf(r, "", "m", msgs, true); k != "" || src != "" {
			t.Fatalf("短首句 %q 不该产生对话键，得到 %q/%q", short, k, src)
		}
	}
	// 但显式 key 不受长度限制
	if k, _ := dialogKeyOf(r, "user-x", "m", []ChatMessage{{Role: "user", Content: json.RawMessage(`"1"`)}}, true); k != "u:user-x" {
		t.Fatalf("显式 user 不该被长度限制影响: %q", k)
	}
}

// system 参与锚点：不同应用即使首句相同也要分开。
func TestDialogKeyIncludesSystem(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	user := ChatMessage{Role: "user", Content: json.RawMessage(`"帮我把这段话翻译成英文，保持语气"`)}

	a := []ChatMessage{{Role: "system", Content: json.RawMessage(`"你是翻译助手"`)}, user}
	b := []ChatMessage{{Role: "system", Content: json.RawMessage(`"你是客服机器人"`)}, user}
	ka, _ := dialogKeyOf(r, "", "m", a, true)
	kb, _ := dialogKeyOf(r, "", "m", b, true)
	if ka == kb {
		t.Fatalf("不同 system 应算出不同键: %q", ka)
	}
	// 同一段对话的第二轮（system 不变）必须同一个键
	c := append(a,
		ChatMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		ChatMessage{Role: "user", Content: json.RawMessage(`"再来一遍"`)})
	if kc, _ := dialogKeyOf(r, "", "m", c, true); kc != ka {
		t.Fatalf("多轮后键变了: %q vs %q", ka, kc)
	}
}

// ---------- 存储 ----------

func TestDialogStoreTTLAndEviction(t *testing.T) {
	d := newDialogStore()
	d.ttl = 10 * time.Millisecond
	d.max = 2

	e := d.bind("k1", "acct-a", "chat-1", "m")
	if e == nil {
		t.Fatal("bind 应返回可写条目")
	}
	e.done()

	if d.peek("k1") == nil {
		t.Fatal("未过期时应能取到")
	}
	time.Sleep(20 * time.Millisecond)
	if d.peek("k1") != nil {
		t.Fatal("过期后应取不到")
	}

	// 上限淘汰：写第 3 条时最老的被清掉
	for _, k := range []string{"a", "b", "c"} {
		if e := d.bind(k, "acct", "chat-"+k, "m"); e != nil {
			e.done()
		}
	}
	d.mu.Lock()
	n := len(d.entries)
	d.mu.Unlock()
	if n > 3 {
		t.Fatalf("未按上限淘汰: %d 条", n)
	}
}

// 同一段对话并发时，只有第一个能拿到使用权；第二个必须拿不到（走换新号）。
func TestDialogStoreBorrowExclusive(t *testing.T) {
	d := newDialogStore()
	if e := d.bind("k", "acct", "chat-1", "m"); e != nil {
		e.done()
	}

	e1 := d.borrow("k", "m")
	if e1 == nil {
		t.Fatal("第一个应拿到")
	}
	if e2 := d.borrow("k", "m"); e2 != nil {
		t.Fatal("第二个不该拿到（并发要降级换新号，不能共用 parentId）")
	}
	e1.done()
	if e3 := d.borrow("k", "m"); e3 == nil {
		t.Fatal("释放后应能再次拿到")
	} else {
		e3.done()
	}

	// 模型不匹配不给复用
	if e := d.borrow("k", "other"); e != nil {
		t.Fatal("模型不一致不该复用")
	}
}

func TestDialogStoreDropAccount(t *testing.T) {
	d := newDialogStore()
	if e := d.bind("a", "acct-x", "c1", "m"); e != nil {
		e.done()
	}
	if e := d.bind("b", "acct-y", "c2", "m"); e != nil {
		e.done()
	}
	d.dropAccount("acct-x")
	if d.peek("a") != nil {
		t.Fatal("该号的对话应被清掉")
	}
	if d.peek("b") == nil {
		t.Fatal("别的号不该被误删")
	}
}

// ---------- 端到端 ----------

func agentReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// 同一个 user 的第二轮必须复用同一个 chat_id，且 parentId 接上一轮的 response_id。
func TestAgentDialogStickyReuse(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	first := agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-1",
		"messages":[{"role":"user","content":"记住数字 7"}]}`)
	h.ServeHTTP(httptest.NewRecorder(), first)
	m1 := up.gotSubmit
	if m1["parentId"] != "" {
		t.Fatalf("首轮 parentId 应为空串，得到 %v", m1["parentId"])
	}

	second := agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-1",
		"messages":[{"role":"user","content":"记住数字 7"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"我刚才让你记什么？"}]}`)
	h.ServeHTTP(httptest.NewRecorder(), second)
	m2 := up.gotSubmit

	if m1["chatId"] != m2["chatId"] {
		t.Fatalf("第二轮应复用同一会话: %v vs %v", m1["chatId"], m2["chatId"])
	}
	if m2["parentId"] != "resp-1" {
		t.Fatalf("第二轮 parentId 应接上一轮 response_id，得到 %v", m2["parentId"])
	}
}

// 不同 user 不能串到同一段对话（这是最危险的错误：泄漏别人的上下文）。
func TestAgentDialogIsolatedByUser(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-a",
		"messages":[{"role":"user","content":"同一句话"}]}`))
	a := up.gotSubmit["chatId"]

	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-b",
		"messages":[{"role":"user","content":"同一句话"}]}`))
	b := up.gotSubmit["chatId"]

	if a == b {
		t.Fatalf("不同 user 串了同一会话: %v", a)
	}
}

// 关掉开关就回到旧行为：每请求新建会话（parentId 恒空）。
func TestAgentDialogDisabled(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	srv := newTestServerWithServer(t, up, 1)
	off := false
	cfg := srv.conf()
	cfg.DialogSticky = &off
	srv.cfg.Store(cfg)
	h := srv.Handler()

	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-1",
			"messages":[{"role":"user","content":"你好"}]}`))
		if got := up.gotSubmit["parentId"]; got != "" {
			t.Fatalf("关掉粘性后 parentId 应为空，得到 %v", got)
		}
	}
}

// 普通模型不受影响：即使带了 user 也不进对话表。
// 普通文本的粘性**只借账号**：同号吃 cache，但会话必须新取、prompt 必须全量。
//
// 2026-11-12 线上实测：上游 t2t 不会把 chat_id 里的历史补进模型输入。
// 发增量时模型答"请问您指的是哪个数的平方"——它根本没看到第一轮，
// 而 cached 反而从 512 涨到 1152（历史进了 prompt 前缀缓存，却不在上下文里）。
func TestPlainModelKeepsAccountButResendsFull(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	s := newTestServerWithServer(t, up, 2)
	h := s.Handler()

	const first = "帮我解释一下 TCP 三次握手为什么必须三次而不是两次"
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.7-plus","stream":false,
		"messages":[{"role":"user","content":"`+first+`"}]}`))
	chat1 := up.gotSubmit["chatId"]

	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.7-plus","stream":false,
		"messages":[{"role":"user","content":"`+first+`"},
		            {"role":"assistant","content":"因为要确认双方收发能力"},
		            {"role":"user","content":"那四次挥手呢"}]}`))

	// 会话必须换新的：复用旧会话只会让 history 写两遍，而上游 t2t 又不读它。
	if got := up.gotSubmit["chatId"]; got == chat1 {
		t.Fatalf("普通文本不应复用会话，实际仍用 %v", got)
	}
	// prompt 必须全量（含历史），否则模型看不到上下文。
	if c := submitContent(t, up); !strings.Contains(c, "对话历史") {
		t.Fatalf("普通文本必须发全量拼接，实际 content=%q", c)
	}
	// 账号要粘住 —— 这是 cache 能命中的唯一来源。
	rows := s.log.Recent(2)
	if len(rows) != 2 || rows[0].Account != rows[1].Account {
		t.Fatalf("同一个对话应固定在同一账号上，得到 %+v", rows)
	}
}

// 普通文本的对话键不能用 user 字段：那是终端用户 ID，一个用户开多个对话会串。
func TestDialogKeySkipsUserForPlain(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: json.RawMessage(`"一段足够长的开场白用于锚点"`)}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	k, src := dialogKeyOf(r, "u-same", "m", msgs, false)
	if src == "user" || k == "u:u-same" {
		t.Fatalf("普通文本不得用 user 做对话键，得到 %q/%q", k, src)
	}
	// Agent 仍然用它（一次一个长任务，语义上就是"这个用户的当前任务"）。
	if _, src := dialogKeyOf(r, "u-same", "m", msgs, true); src != "user" {
		t.Fatalf("Agent 应仍优先 user，得到 %q", src)
	}
}

// 完全不传 user / X-Dialog-Id 时，靠"首条长消息"的 hash 也要能粘住。
// 这是零改造路径 —— 多数现有客户端就长这样。
func TestAgentDialogHashSticky(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	const first = "帮我写一个 Python 脚本，把目录下所有 png 批量转成 webp"
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,
		"messages":[{"role":"user","content":"`+first+`"}]}`))
	a := up.gotSubmit["chatId"]

	// 第二轮：同一首句 + 历史 + 新问题
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,
		"messages":[{"role":"user","content":"`+first+`"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"再加个 --quality 参数"}]}`))
	b := up.gotSubmit["chatId"]

	if a != b {
		t.Fatalf("hash 兜底未复用同一会话: %v vs %v", a, b)
	}
	if up.gotSubmit["parentId"] != "resp-1" {
		t.Fatalf("parentId 未接上: %v", up.gotSubmit["parentId"])
	}
}

// 粘性命中只发本轮增量；未命中必须回退"拼接历史"（旧版本行为）。
//
// 这条守的是 2026-11-12 用户提的语义：粘性只是省 token 的优化，
// 一旦失败就得回到全量拼接 —— 上下文由我们自己保证，不依赖上游会话。
func TestAgentDialogDeltaPrompt(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	// 首轮（未复用）：即使客户端已经带了历史，也必须发全量拼接。
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-delta",
		"messages":[{"role":"user","content":"记住数字 7"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"7 的平方是多少"}]}`))
	first := submitContent(t, up)
	if !strings.Contains(first, "对话历史") || !strings.Contains(first, "记住数字 7") {
		t.Fatalf("首轮必须发全量拼接（含历史），实际 content=%q", first)
	}

	// 第二轮（复用）：只发本轮输入，历史交给上游会话链。
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-delta",
		"messages":[{"role":"user","content":"记住数字 7"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"7 的平方是多少"},
		            {"role":"assistant","content":"49"},
		            {"role":"user","content":"再加 1 呢"}]}`))
	second := submitContent(t, up)
	if second != "再加 1 呢" {
		t.Fatalf("粘性命中只应发本轮增量，实际 content=%q", second)
	}
}

// 同一 user 但换了模型 ⇒ 不复用会话 ⇒ 必须回到全量拼接（不能发增量）。
func TestAgentDialogDeltaFallsBackOnNewChat(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-fb",
		"messages":[{"role":"user","content":"记住数字 8"}]}`))
	// 第二次换 user ⇒ 不同对话 ⇒ 新会话，仍要全量。
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-other",
		"messages":[{"role":"user","content":"记住数字 8"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"8 的立方"}]}`))
	got := submitContent(t, up)
	if !strings.Contains(got, "对话历史") {
		t.Fatalf("未复用会话时必须回退全量拼接，实际 content=%q", got)
	}
}

// submitContent 取最后一次提交的 messages[0].content。
func submitContent(t *testing.T, up *fakeUpstream) string {
	t.Helper()
	if up.gotSubmit == nil {
		t.Fatal("没抓到 completions 请求体")
	}
	msgs, _ := up.gotSubmit["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatalf("messages 为空: %v", up.gotSubmit)
	}
	msg, _ := msgs[0].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

// 粘性失败后必须把那段对话从表里删掉，否则下一轮会 borrow 到同一个坏会话
// 再撞一次 —— 这正是"报错了就回退负载其他号"缺的那一半。
//
// 之前 dialogEntry.created 注释写着"失败时要回收"，但那个字段**从没被读过**，
// 等于失效会话永远留在表里（2026-11-12 修）。
func TestDialogDroppedOnFailure(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	s := newTestServerWithServer(t, up, 1)
	h := s.Handler()

	// 第一轮成功 ⇒ 绑定一段对话。
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-drop",
		"messages":[{"role":"user","content":"记住数字 9"}]}`))
	if got := s.dialogs.snapshot()["entries"].(int); got != 1 {
		t.Fatalf("首轮成功后应有 1 段对话，得到 %d", got)
	}

	// 第二轮：上游 500（快速失败，会换号重试但池里只有 1 个号）⇒ 整轮失败。
	up.fail500 = 8
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-drop",
		"messages":[{"role":"user","content":"记住数字 9"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"9 的平方"}]}`))

	if got := s.dialogs.snapshot()["entries"].(int); got != 0 {
		t.Fatalf("失败后对话应被回收，仍残留 %d 条", got)
	}
	up.fail500 = 0

	// 第三轮：必须是新会话（dlg=new），不能复用坏的那个。
	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.8-flash-agent","stream":false,"user":"u-drop",
		"messages":[{"role":"user","content":"记住数字 9"},
		            {"role":"assistant","content":"好"},
		            {"role":"user","content":"9 的平方"}]}`))
	if got := up.gotSubmit["chatId"]; got == "chat-1" {
		t.Fatalf("失败后应换新会话，实际仍用 chat-1")
	}
}
