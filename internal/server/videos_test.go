package server

// 视频前门的端到端测试（此前一条都没有 —— 视频是**花钱**的动作，
// 而充值/余额这些分支恰恰是"看起来通、实际误导"的重灾区）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// videoUpstream 假上游：建会话 + 提交任务 + 积分余额。
type videoUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// balance 返回给网关的积分余额。
	balance int
	// creditsFail 为 true 时积分接口 500（模拟读不到余额）。
	creditsFail bool
	submits     int
}

func newVideoUpstream(t *testing.T, balance int) *videoUpstream {
	t.Helper()
	f := &videoUpstream{balance: balance}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/entitlement/credits/credits/history"):
			f.mu.Lock()
			fail, bal := f.creditsFail, f.balance
			f.mu.Unlock()
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"httpCode":200,"success":true,"data":{"totalCredits":` +
				itoa(bal) + `,"subscriptionCredits":0,"bonusCredits":0}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chats/new"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chat-1"}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			f.mu.Lock()
			f.submits++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"messages":[
				{"extra":{"wanx":{"task_id":"tsk_test123"}}}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *videoUpstream) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits
}

func newVideoServer(t *testing.T, up *videoUpstream, mutate func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.Upstream.EntitlementBaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	if mutate != nil {
		mutate(&cfg)
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test").Handler()
}

func postVideo(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(body)))
	return rec
}

func TestVideoRejectedWhenBalanceTooLow(t *testing.T) {
	// 核心场景：余额 10 分，请求 720P 5s（30 分）。
	// 必须本地就拦掉 —— 放过去只会拿回「Credits 不足 当前可用 10 Credits」，
	// 白跑一次上游，还可能把账号记成一次失败。
	up := newVideoUpstream(t, 10)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, `{"prompt":"猫","resolution":"720P","duration":5}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("余额不足应 429，得到 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "insufficient_quota") {
		t.Fatalf("错误类型应为 insufficient_quota: %s", body)
	}
	if !strings.Contains(body, "余额不足") || !strings.Contains(body, "a0=10") {
		t.Fatalf("文案应说明具体余额（否则用户不知道该充多少）: %s", body)
	}
	if !strings.Contains(body, "需要 30") {
		t.Fatalf("文案应给出本次所需积分: %s", body)
	}
	if n := up.submitCount(); n != 0 {
		t.Fatalf("余额不足时不该提交上游，实际提交 %d 次", n)
	}
}

func TestVideoSucceedsAndDebitsBalance(t *testing.T) {
	// 余额 100，做一次 720P 5s（30）⇒ 200，且本地余额立刻变 70（乐观扣减）。
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, `{"prompt":"猫","resolution":"720P","duration":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var obj VideoObject
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if obj.ID == "" || obj.UpstreamID != "tsk_test123" {
		t.Fatalf("返回体不对: %+v", obj)
	}
	if n := up.submitCount(); n != 1 {
		t.Fatalf("应提交 1 次，实际 %d", n)
	}

	// /stats 里应看到余额已被扣到 70
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if !strings.Contains(srec.Body.String(), `"credit_balance":70`) {
		t.Fatalf("乐观扣减未生效: %s", srec.Body.String()[:400])
	}
}

func TestVideoAllowedWhenBalanceUnreadable(t *testing.T) {
	// 读不到余额时必须**放行**，靠上游 insufficient_quota 兜底。
	// 反过来（默认拦死）等于一次接口抖动就停掉整个视频能力。
	up := newVideoUpstream(t, 0)
	up.creditsFail = true
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, `{"prompt":"猫","resolution":"720P","duration":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("余额读不到时应放行，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := up.submitCount(); n != 1 {
		t.Fatalf("应提交 1 次，实际 %d", n)
	}
}

func TestVideoRejectsUnknownTier(t *testing.T) {
	// 7 秒不是上游支持的档位（只有 5/10/15）。这种请求不该放过去烧积分。
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, `{"prompt":"猫","resolution":"720P","duration":7}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法档位应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := up.submitCount(); n != 0 {
		t.Fatalf("非法档位不该提交上游，实际 %d", n)
	}
}

func TestVideoTierPricingMatchesModelVersion(t *testing.T) {
	// 价格表跟着 root_model 走：happyhorse11（默认）720P 5s=30、1080P 15s=120；
	// 1.0 是 40/210。这条测试钉住"别拿错表"这个坑。
	cfg := config.Default()
	cases := []struct {
		res  string
		dur  int
		want int
	}{
		{"720P", 5, 30}, {"720P", 10, 60}, {"720P", 15, 90},
		{"1080P", 5, 40}, {"1080P", 10, 80}, {"1080P", 15, 120},
	}
	for _, c := range cases {
		got, ok := cfg.Video.VideoCredits(c.res, c.dur)
		if !ok || got != c.want {
			t.Fatalf("%s/%ds 应为 %d 分，得到 %d（ok=%v）", c.res, c.dur, c.want, got, ok)
		}
	}
	if cfg.Video.RootModel != "happyhorse11" {
		t.Fatalf("默认 root_model 变了（%s）⇒ 价格表要跟着换", cfg.Video.RootModel)
	}
}
