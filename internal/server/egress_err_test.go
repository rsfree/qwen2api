package server

// 客户端可见错误 + 出口 IP 的行为约束。
//
// 这两件事都是 2026-10-30 用户明确要求的：
//  1. 上游故障/超时对客户端只回**标准英文 5xx**（中文排查指南留在流水里）；
//  2. 请求流水要能看出"这次是哪个账号、从哪个出口 IP 出去的"。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 5xx 一律英文标准文案；4xx/429 保留原文（那儿的具体信息对调用方有用）。
func TestClientErrorStandardEnglish(t *testing.T) {
	cases := []struct {
		code       int
		typ, msg   string
		wantType   string
		wantMsg    string
		wantChange bool
	}{
		{504, "upstream_timeout", "上游 180s 内没有任何产物（只发心跳）。两种可能：①额度将尽…",
			"upstream_timeout", "upstream timeout, please retry later", true},
		{502, "upstream_error", "上游没有返回任何图片（既没有 image_gen 产物帧…）",
			"upstream_error", "upstream error, please retry later", true},
		{503, "service_unavailable", "没有可用账号：全部冷却中",
			"service_unavailable", "no available upstream account, please retry later", true},
		{502, "upstream_auth_error", "账号 a1 的 access token 失效",
			"upstream_error", "upstream authentication failed", true},
		// 4xx / 429：原样（这是"你写错了"或"哪个额度不够"，具体才省事）
		{400, "invalid_request_error", "image 最多 20 张", "invalid_request_error", "image 最多 20 张", false},
		{429, "insufficient_quota", "全部 3135 个账号今日出图额度已用尽（本地限额 6 张/号/天）",
			"insufficient_quota", "全部 3135 个账号今日出图额度已用尽（本地限额 6 张/号/天）", false},
	}
	for _, c := range cases {
		typ, msg := clientError(c.code, c.typ, c.msg)
		if typ != c.wantType || msg != c.wantMsg {
			t.Fatalf("%d %s: 得到 (%s, %q)，期望 (%s, %q)", c.code, c.typ,
				typ, msg, c.wantType, c.wantMsg)
		}
		if c.wantChange && strings.IndexFunc(msg, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
			t.Fatalf("5xx 文案不该含中文: %q", msg)
		}
	}
}

// 心跳超时要落成 504 + upstream_timeout（客户端/SDK 才能区分"超时"与"故障"）。
func TestHeartbeatTimeoutMapsTo504(t *testing.T) {
	err := errUpstreamTimeout
	if got := httpStatusFor(err); got != http.StatusGatewayTimeout {
		t.Fatalf("状态码应为 504，得到 %d", got)
	}
	if got := errTypeFor(err); got != "upstream_timeout" {
		t.Fatalf("type 应为 upstream_timeout，得到 %s", got)
	}
	// 包了上下文也要认（generateImage 用 %w 附加了"两种可能"的说明）。
	wrapped := fmt.Errorf("%w。两种可能：额度将尽或上游抖动", err)
	if got := httpStatusFor(wrapped); got != http.StatusGatewayTimeout {
		t.Fatalf("包装后仍应 504，得到 %d", got)
	}
}

// 上游探测：返回纯文本 IP；异常返回要报错；"-" 关闭。
func TestEgressIPProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("203.0.113.7\n"))
	}))
	defer ok.Close()

	cfg := config.Default().Upstream
	cfg.EgressIPURL = ok.URL
	c := upstream.New(cfg)
	ip, err := c.EgressIP(context.Background(), "a0")
	if err != nil || ip != "203.0.113.7" {
		t.Fatalf("探测结果 (%q, %v)，期望 203.0.113.7", ip, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not an ip</html>"))
	}))
	defer bad.Close()
	cfg2 := config.Default().Upstream
	cfg2.EgressIPURL = bad.URL
	if _, err := upstream.New(cfg2).EgressIP(context.Background(), "a0"); err == nil {
		t.Fatal("非 IP 响应应报错（不能让脏值进流水）")
	}

	cfg3 := config.Default().Upstream
	cfg3.EgressIPURL = "-"
	if _, err := upstream.New(cfg3).EgressIP(context.Background(), "a0"); err == nil {
		t.Fatal("egress_ip_url=\"-\" 时应明确拒绝")
	}
}

// 请求流水必须带上账号 + 出口 IP（"谁调的 / 哪个号 / 从哪出去"三件套齐全）。
func TestRequestLogCarriesEgressIP(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)
	s.pool.SetEgressIP("a0", "198.51.100.9")

	h := s.Handler()
	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("请求应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}

	rows := s.log.Recent(1)
	if len(rows) != 1 {
		t.Fatalf("流水条数 %d", len(rows))
	}
	if rows[0].Account != "a0" {
		t.Fatalf("流水应记账号 a0，得到 %q", rows[0].Account)
	}
	if rows[0].EgressIP != "198.51.100.9" {
		t.Fatalf("流水应带出口 IP 198.51.100.9，得到 %q", rows[0].EgressIP)
	}
}

// 池里的出口 IP 有有效期：过期就该当未知（否则面板会长期显示一个漂走的值）。
func TestPoolEgressIPExpires(t *testing.T) {
	p := pool.New(config.Default().Pool, []config.Account{{Name: "a0", Token: "t"}}, "")
	p.SetEgressIP("a0", "198.51.100.9")
	if got := p.EgressIP("a0", time.Hour); got != "198.51.100.9" {
		t.Fatalf("刚写入应可见，得到 %q", got)
	}
	if got := p.EgressIP("a0", time.Nanosecond); got != "" {
		t.Fatalf("超过 maxAge 应视为未知，得到 %q", got)
	}
	if got := p.EgressIP("ghost", time.Hour); got != "" {
		t.Fatalf("不存在的账号应回空，得到 %q", got)
	}
}

// 图片失败时也要记账号（2026-10-30 线上出现过 acct= 空的 502，查不到是哪个号）。
func TestImageFailureStillRecordsAccount(t *testing.T) {
	up := newFakeUpstream(t, 1) // 第一次调用直接 401，且没有 refresh_token 可用
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	h := s.Handler()

	rec := httptest.NewRecorder()
	body := `{"model":"qwen-image-3.0","prompt":"画一只猫","image":"https://cdn.example.com/a.png"}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))

	if rec.Code < 500 {
		t.Fatalf("应以上游失败收场（得到 %d）: %s", rec.Code, rec.Body.String())
	}
	rows := s.log.Recent(1)
	if len(rows) != 1 {
		t.Fatalf("流水条数 %d", len(rows))
	}
	if rows[0].Account != "a0" {
		t.Fatalf("失败也要记账号，得到 %q（线上曾因此查不到是哪个号）", rows[0].Account)
	}
	// 客户端只该看到标准英文。
	if strings.Contains(rows[0].Error, "upstream") {
		// rows[0].Error 是给运维的中文细节，允许任意；这里只确认响应体是英文。
	}
	if strings.Contains(rec.Body.String(), "上游") {
		t.Fatalf("5xx 响应体不该含中文: %s", rec.Body.String())
	}
}

// 出图预算随垫图数量放宽（实测每多一张约 +18s，180s 基线对 3 张偏紧）。
func TestImageTimeoutScalesWithRefs(t *testing.T) {
	cases := []struct {
		refs int
		want time.Duration
	}{
		{0, 150 * time.Second}, {1, 150 * time.Second}, {2, 170 * time.Second},
		{3, 190 * time.Second}, {6, 240 * time.Second}, {20, 240 * time.Second},
	}
	for _, c := range cases {
		if got := imageTimeout(c.refs); got != c.want {
			t.Fatalf("imageTimeout(%d) = %s，期望 %s", c.refs, got, c.want)
		}
	}
}

// 流水要记垫图张数：判断"是不是垫图拖慢/超时"时这是第一个要看的东西。
func TestRequestLogRecordsRefImageCount(t *testing.T) {
	up := newFakeUpstream(t, 0)
	s := newTestServerWithServer(t, up, 1)
	h := s.Handler()

	body := `{"model":"qwen-image-3.0","prompt":"把这三张拼一起","image":[
		"https://cdn.example.com/1.png","https://cdn.example.com/2.png","https://cdn.example.com/3.png"]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))

	rows := s.log.Recent(1)
	if len(rows) != 1 {
		t.Fatalf("流水条数 %d", len(rows))
	}
	if rows[0].RefImages != 3 {
		t.Fatalf("垫图张数应记 3，得到 %d", rows[0].RefImages)
	}
	if rows[0].Account != "a0" {
		t.Fatalf("账号应记 a0，得到 %q", rows[0].Account)
	}
}

// "账号不能用"（未激活/封禁）不能落成 4xx 甩给调用方：换号 + 停用该号，最终 503。
func TestAccountNotReadyMapsTo503(t *testing.T) {
	err := upstream.NewErr(upstream.KindAuth, "chats/new", 400, upstream.CodeAccountNotReady,
		"Your account is currently pending activation.", true)
	if got := httpStatusFor(err); got != http.StatusServiceUnavailable {
		t.Fatalf("状态码应 503，得到 %d", got)
	}
	if got := errTypeFor(err); got != "service_unavailable" {
		t.Fatalf("type 应 service_unavailable，得到 %s", got)
	}
	typ, msg := clientError(httpStatusFor(err), errTypeFor(err), err.Error())
	if typ != "service_unavailable" || msg != "no available upstream account, please retry later" {
		t.Fatalf("文案不对: (%s, %q)", typ, msg)
	}
}

// 聊天失败也要记账号（否则 acct= 空，连带出口 IP 一起丢）。
func TestChatFailureRecordsAccount(t *testing.T) {
	up := newFakeUpstream(t, 9) // 全部 401，且没有 refresh_token
	s := newTestServerWithServer(t, up, 1)
	s.pool.SetEgressIP("a0", "198.51.100.9")
	h := s.Handler()

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	rows := s.log.Recent(1)
	if len(rows) != 1 {
		t.Fatalf("流水条数 %d", len(rows))
	}
	if rows[0].Account != "a0" {
		t.Fatalf("失败也要记账号，得到 %q", rows[0].Account)
	}
	if rows[0].EgressIP != "198.51.100.9" {
		t.Fatalf("失败也要带出口 IP，得到 %q", rows[0].EgressIP)
	}
}

// 换号判据：只有"上游没受理"的失败才重试（零产物超时 / RetrySafe），
// 其它可能已受理，重试=赌重复扣费。
func TestRetryableImageErr(t *testing.T) {
	if !retryableImageErr(errUpstreamTimeout) {
		t.Fatal("零产物超时应换号")
	}
	if !retryableImageErr(fmt.Errorf("%w（预算 150s）", errUpstreamTimeout)) {
		t.Fatal("包装后的超时也应换号")
	}
	// 账号级失败（401 / 额度）语义是"未受理" ⇒ 换号安全。
	if !retryableImageErr(upstream.NewErr(upstream.KindAuth, "chats/new", 401, "Unauthorized", "x", true)) {
		t.Fatal("401 应换号")
	}
	if !retryableImageErr(upstream.NewErr(upstream.KindQuota, "chat", 429, "RateLimited", "quota", true)) {
		t.Fatal("额度类应换号")
	}
	// 可能已受理的失败不换。
	if retryableImageErr(upstream.NewErr(upstream.KindServer, "chat", 500, "", "internal_error", false)) {
		t.Fatal("5xx 可能已受理，不该换号")
	}
	if retryableImageErr(errors.New("random")) || retryableImageErr(nil) {
		t.Fatal("其它错误不换号")
	}
}

// 超时后要给这个号软冷却，否则下一轮还会选中它、再白等一轮。
func TestImageTimeoutCoolsAccount(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(5), "test")

	a := p.Get("a0")
	s.coolAfterImageTimeout(a)

	state := ""
	for _, v := range p.Snapshot() {
		if v.Name == "a0" {
			state = v.State
		}
	}
	if state == "ready" {
		t.Fatalf("超时的号应进入冷却，实际状态 %q", state)
	}
}

// 生图遇到**账号级**失败要换号（原来一个坏号就把请求判死：503）。
func TestImageRotatesOnAccountLevelFailure(t *testing.T) {
	up := newFakeUpstream(t, 1) // 第一次调用 401，之后正常（假上游只回文本帧 ⇒ 最终无图）
	s := newTestServerWithServer(t, up, 2)
	h := s.Handler()

	body := `{"model":"qwen-image-3.0","prompt":"画一只猫"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))

	if up.calls < 2 {
		t.Fatalf("应在第一个号 401 后换号重试，实际只调了 %d 次上游", up.calls)
	}
	rows := s.log.Recent(1)
	if len(rows) != 1 {
		t.Fatalf("流水条数 %d", len(rows))
	}
	if rows[0].Account != "a1" {
		t.Fatalf("换号后应记第二个号 a1，得到 %q", rows[0].Account)
	}
}
