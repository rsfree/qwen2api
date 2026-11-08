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
	if k, src := dialogKeyOf(r, "u1", "m", msgs); src != "header" || k != "h:abc" {
		t.Fatalf("头优先失败: %q %q", k, src)
	}

	// 2. user 次之
	r.Header.Del(dialogHeader)
	if k, src := dialogKeyOf(r, "u1", "m", msgs); src != "user" || k != "u:u1" {
		t.Fatalf("user 次优失败: %q %q", k, src)
	}

	// 3. 都没有 → 首条用户消息的 hash
	k1, src := dialogKeyOf(r, "", "m", msgs)
	if src != "first_message" || !strings.HasPrefix(k1, "f:") {
		t.Fatalf("hash 兜底失败: %q %q", k1, src)
	}
	// 同一段对话的第二轮：messages 变长了，但首条没变 ⇒ 键必须一样
	msgs2 := append(msgs,
		ChatMessage{Role: "assistant", Content: json.RawMessage(`"好"`)},
		ChatMessage{Role: "user", Content: json.RawMessage(`"继续"`)})
	if k2, _ := dialogKeyOf(r, "", "m", msgs2); k2 != k1 {
		t.Fatalf("首条不变但键变了: %q vs %q", k1, k2)
	}
	// 换模型 ⇒ 换键（上游会话的 models[] 建的时候就定死了）
	if k3, _ := dialogKeyOf(r, "", "other-model", msgs); k3 == k1 {
		t.Fatal("换模型应换键")
	}
	// 没有 user 消息 ⇒ 不做粘性
	empty := []ChatMessage{{Role: "system", Content: json.RawMessage(`"sys"`)}}
	if k, src := dialogKeyOf(r, "", "m", empty); k != "" || src != "" {
		t.Fatalf("无用户消息应返回空键: %q %q", k, src)
	}
}

// 短首句不能当锚点：真实流量里"你好/1/继续"高度重复，撞键会串对话。
func TestDialogKeyRejectsShortAnchor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for _, short := range []string{"你好", "1", "继续", "在吗", "ok"} {
		msgs := []ChatMessage{{Role: "user", Content: json.RawMessage(`"` + short + `"`)}}
		if k, src := dialogKeyOf(r, "", "m", msgs); k != "" || src != "" {
			t.Fatalf("短首句 %q 不该产生对话键，得到 %q/%q", short, k, src)
		}
	}
	// 但显式 key 不受长度限制
	if k, _ := dialogKeyOf(r, "user-x", "m", []ChatMessage{{Role: "user", Content: json.RawMessage(`"1"`)}}); k != "u:user-x" {
		t.Fatalf("显式 user 不该被长度限制影响: %q", k)
	}
}

// system 参与锚点：不同应用即使首句相同也要分开。
func TestDialogKeyIncludesSystem(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	user := ChatMessage{Role: "user", Content: json.RawMessage(`"帮我把这段话翻译成英文，保持语气"`)}

	a := []ChatMessage{{Role: "system", Content: json.RawMessage(`"你是翻译助手"`)}, user}
	b := []ChatMessage{{Role: "system", Content: json.RawMessage(`"你是客服机器人"`)}, user}
	ka, _ := dialogKeyOf(r, "", "m", a)
	kb, _ := dialogKeyOf(r, "", "m", b)
	if ka == kb {
		t.Fatalf("不同 system 应算出不同键: %q", ka)
	}
	// 同一段对话的第二轮（system 不变）必须同一个键
	c := append(a,
		ChatMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
		ChatMessage{Role: "user", Content: json.RawMessage(`"再来一遍"`)})
	if kc, _ := dialogKeyOf(r, "", "m", c); kc != ka {
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
func TestPlainModelIgnoresDialog(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	h := newTestServer(t, up, 2, nil)

	h.ServeHTTP(httptest.NewRecorder(), agentReq(`{"model":"qwen3.7-plus","stream":false,"user":"u-1",
		"messages":[{"role":"user","content":"你好"}]}`))
	if got := up.gotSubmit["parentId"]; got != "" {
		t.Fatalf("普通模型不该有 parentId: %v", got)
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
