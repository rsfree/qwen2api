package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 池子本身的行为 ----------

func TestChatPoolTakeOnce(t *testing.T) {
	p := NewChatPool(2, time.Minute)
	if !p.Put("a0", "t2t", "chat-1") {
		t.Fatal("Put 应该成功")
	}
	id, ok := p.Take("a0", "t2t")
	if !ok || id != "chat-1" {
		t.Fatalf("应取到 chat-1，得到 %q %v", id, ok)
	}
	// 取出即删：会话用过就有消息了，不能复用（复用会变成多轮对话）。
	if _, ok := p.Take("a0", "t2t"); ok {
		t.Fatal("同一个会话不该被取两次")
	}
}

func TestChatPoolIsolatesAccountAndType(t *testing.T) {
	p := NewChatPool(2, time.Minute)
	p.Put("a0", "t2t", "chat-a0")

	// 🔴 会话是账号维度资源：跨号取必须为空（上游会 CHAT_NOT_FOUND）。
	if _, ok := p.Take("a1", "t2t"); ok {
		t.Fatal("会话不能跨账号复用")
	}
	// 会话类型不同也不能混用（建的时候类型就定了）。
	if _, ok := p.Take("a0", "t2i"); ok {
		t.Fatal("t2t 的会话不能当 t2i 用")
	}
	if id, ok := p.Take("a0", "t2t"); !ok || id != "chat-a0" {
		t.Fatalf("原号原类型应还能取到，得到 %q %v", id, ok)
	}
}

func TestChatPoolTTL(t *testing.T) {
	p := NewChatPool(2, time.Minute)
	now := time.Now()
	p.now = func() time.Time { return now }
	p.Put("a0", "t2t", "chat-old")

	// 越过 TTL 的会话不能再拿去提交：上游是否会让空会话过期没有实测依据，
	// 但拿一个很旧的会话去提交是没必要的赌博。
	p.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, ok := p.Take("a0", "t2t"); ok {
		t.Fatal("过期会话不该被取到")
	}
	if n := p.Trim(); n != 0 {
		t.Fatalf("Take 已顺手清掉，Trim 应为 0，得到 %d", n)
	}
}

func TestChatPoolPerLimit(t *testing.T) {
	p := NewChatPool(1, time.Minute)
	if !p.Put("a0", "t2t", "c1") {
		t.Fatal("第一个应成功")
	}
	if p.Put("a0", "t2t", "c2") {
		t.Fatal("超过 per 上限不该再存（否则池子会无限长）")
	}
	if p.Short("a0", "t2t") != 0 {
		t.Fatalf("已满时 Short 应为 0，得到 %d", p.Short("a0", "t2t"))
	}
}

func TestChatPoolDisabled(t *testing.T) {
	p := NewChatPool(0, time.Minute) // warm_chats_per_account = 0
	if p.Enabled() {
		t.Fatal("per=0 应视为关闭")
	}
	if p.Put("a0", "t2t", "c1") {
		t.Fatal("关闭时不该存")
	}
	if _, ok := p.Take("a0", "t2t"); ok {
		t.Fatal("关闭时不该取到")
	}
}

// ---------- 端到端：命中池子就少打一次上游 ----------

// 命中预建会话时，请求路径上不该再调 chats/new（省 2.0~2.8 秒）。
func TestChatHitSkipsNewChatCall(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)
	// per=1：命中后池子会空，此时**只允许后台补货**，请求路径上不许等它。
	// 用热更接口改配置（conf() 返回的是快照，不能就地赋值）。
	cfg := s.conf()
	cfg.Pool.WarmChatsPerAccount = 1
	s.ApplyConfig(cfg)
	s.chats = NewChatPool(1, time.Minute)
	s.chats.Put("a0", "t2t", "chat-pre")

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 提交必须用池里那一条 —— 这是"命中生效"的直接证据。
	if got, _ := up.gotSubmit["chat_id"].(string); got != "chat-pre" {
		t.Fatalf("提交用的 chat_id 应为 chat-pre，得到 %q", got)
	}
	// 命中会把池子抽空，所以**后台**要补一个（否则下一次必然 miss，
	// 命中率被锁死在 50%）。补货是异步的，允许它晚一点发生。
	deadline := time.Now().Add(time.Second)
	for up.newChats == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if up.newChats != 1 {
		t.Fatalf("命中后应在后台补 1 个会话，实际 %d 次", up.newChats)
	}
}

// 池里没有就现建 —— 功能不能依赖预热成功。
func TestChatMissFallsBackToNewChat(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)
	// 用热更接口改配置（conf() 返回的是快照，不能就地赋值）。
	cfg := s.conf()
	cfg.Pool.WarmChatsPerAccount = 1
	s.ApplyConfig(cfg)
	s.chats = NewChatPool(1, time.Minute) // 空池

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if up.newChats < 1 {
		t.Fatalf("池空时必须现建会话，实际 chats/new 调用 %d 次", up.newChats)
	}
}

// 出图/出视频不维护池子（一次 50~90s，建会话 2.4s 占比可忽略），
// 命中不了也要能正常走现建 —— 不能因为池子是空就报错。
func TestImageStillWorksWithoutChatPool(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)

	body := `{"model":"qwen-image-3.0","prompt":"画一只猫"}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))
	if rec.Code == 0 {
		t.Fatal("不该连响应都没有")
	}
	if up.newChats < 1 {
		t.Fatalf("出图应现建会话，实际 chats/new %d 次", up.newChats)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
}

// 命中率观测：命中=省掉一次 2.4s 的 chats/new。miss 偏高说明预热覆盖的号
// 与实际选号错位（warmTarget 太小或 HotNames 排序不一致）。
func TestChatPoolHitRateStat(t *testing.T) {
	p := NewChatPool(1, time.Minute)
	p.Put("a0", "t2t", "c1")
	p.Take("a0", "t2t") // 命中
	p.Take("a0", "t2t") // 未命中（取出即删）
	p.Take("a1", "t2t") // 未命中（跨号）

	st := p.Stat()
	if st.Hit != 1 || st.Miss != 2 {
		t.Fatalf("hit/miss 统计不对: hit=%d miss=%d", st.Hit, st.Miss)
	}
	if st.HitPct < 33 || st.HitPct > 34 {
		t.Fatalf("命中率应约 33.3%%，得到 %.1f", st.HitPct)
	}
	if st.Ready != 0 {
		t.Fatalf("取完应剩 0，得到 %d", st.Ready)
	}
}

// 补货名额：同一个号同一时刻只允许一个在途补货。
//
// 没有它的话，突发流量下每个命中都会触发一次 chats/new，
// 而 Put 的 per 上限又把多余的丢掉 —— 上游调用量翻倍、全白打。
func TestChatPoolTryBuildSingleFlight(t *testing.T) {
	p := NewChatPool(1, time.Minute)
	if !p.TryBuild("a0", "t2t") {
		t.Fatal("第一次应拿到名额")
	}
	if p.TryBuild("a0", "t2t") {
		t.Fatal("在途时不该再拿到第二个名额")
	}
	if !p.TryBuild("a1", "t2t") {
		t.Fatal("不同账号互不影响")
	}
	p.BuildDone("a0", "t2t")
	if !p.TryBuild("a0", "t2t") {
		t.Fatal("释放后应能重新拿到")
	}
	// 关闭预建时一律不发名额：否则会往一个没人用的池子里灌会话。
	if NewChatPool(0, time.Minute).TryBuild("a0", "t2t") {
		t.Fatal("per=0 时不该发名额")
	}
}
