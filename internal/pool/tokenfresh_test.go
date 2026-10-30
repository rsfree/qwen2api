package pool

// 大池子的选号与预热调度。
//
// 目标：负载在整池均匀摊开（用得最少的先上），同时让"下一步要用的那批"
// 凭据提前变新，请求路径上不用等续期。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestPickEvenlyByServed(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
		{Name: "c", Token: "bearer:c"},
	}, "")

	// 每轮"取 → 成功 → 释放"，统计 9 次的选择顺序。
	// served 只在 MarkOK（请求真的成功）时 +1，所以失败不会把号"用旧"。
	var got []string
	for i := 0; i < 9; i++ {
		a, ok := p.Pick()
		if !ok {
			t.Fatal("应能选出账号")
		}
		got = append(got, a.Name)
		p.MarkOK(a)
		p.Release(a)
	}
	want := []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 次取到 %s，期望 %s（顺序=%v）", i+1, got[i], want[i], got)
		}
	}
}

func TestPickTieBreaksByInflight(t *testing.T) {
	// served 相同时优先"在途最少"：并发场景下不能把请求堆到同一个号上。
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
	}, "")
	a1, _ := p.Pick()
	a2, _ := p.Pick()
	if a1.Name == a2.Name {
		t.Fatalf("第二个请求应选另一个号，实际都选了 %s", a1.Name)
	}
}

func TestWarmCandidatesFollowsUseOrder(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "hot", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "cold", Token: "bearer:b", RefreshToken: "rt-b"},
		{Name: "norefresh", Token: "bearer:c"},
	}, "")
	soon := time.Now().Add(time.Minute)
	p.mu.Lock()
	p.accounts[0].served = 5
	p.accounts[0].tokenExp = soon
	p.accounts[1].served = 0
	p.accounts[1].tokenExp = soon
	p.accounts[2].served = 0
	p.accounts[2].tokenExp = soon
	p.mu.Unlock()

	// 没有 refresh_token 的号根本进不了候选（预热只能靠 refresh）。
	// target=1 时只覆盖"用得最少"的那个：cold。
	if names := p.WarmCandidates(1, 3*time.Minute, 8); len(names) != 1 || names[0] != "cold" {
		t.Fatalf("target=1 应只预热 cold，实际 %v", names)
	}
	// target=2 时按 served 升序补上 hot（它用得比 cold 多，排在后面）。
	names := p.WarmCandidates(2, 3*time.Minute, 8)
	if len(names) != 2 || names[0] != "cold" || names[1] != "hot" {
		t.Fatalf("target=2 应按使用次数排序 [cold hot]，实际 %v", names)
	}
}

func TestWarmCandidatesSkipsFreshTokens(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a", RefreshToken: "rt-a"},
	}, "")
	p.mu.Lock()
	p.accounts[0].tokenExp = time.Now().Add(30 * time.Minute) // 还早
	p.mu.Unlock()

	if names := p.WarmCandidates(8, 3*time.Minute, 8); len(names) != 0 {
		t.Fatalf("凭据还新鲜时不该预热，实际 %v", names)
	}

	p.mu.Lock()
	p.accounts[0].tokenExp = time.Now().Add(-time.Minute) // 已过期
	p.mu.Unlock()
	if names := p.WarmCandidates(8, 3*time.Minute, 8); len(names) != 1 {
		t.Fatalf("已过期的凭据应被预热，实际 %v", names)
	}
}

func TestWarmCandidatesRespectsLimit(t *testing.T) {
	accs := make([]config.Account, 0, 10)
	for i := 0; i < 10; i++ {
		accs = append(accs, config.Account{
			Name: fmt.Sprintf("a%02d", i), Token: "bearer:x",
			RefreshToken: "rt",
		})
	}
	p := New(config.Default().Pool, accs, "")
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenExp = time.Now().Add(-time.Minute)
	}
	p.mu.Unlock()

	if names := p.WarmCandidates(10, 3*time.Minute, 3); len(names) != 3 {
		t.Fatalf("单轮上限应生效，实际 %d 个", len(names))
	}
}

func TestSetTokenParsesExpiry(t *testing.T) {
	// 过期时间从 JWT 的 exp 解出来，而不是猜一个 TTL。
	exp := time.Now().Add(900 * time.Second).Unix()
	tok := fakeJWT(t, exp)

	p := New(config.Default().Pool, []config.Account{{Name: "a"}}, "")
	p.SetToken("a", "bearer:"+tok)
	got, ok := p.TokenExpiry("a")
	if !ok || got.Unix() != exp {
		t.Fatalf("应解出 exp=%d，实际 %v ok=%v", exp, got, ok)
	}
}

// fakeJWT 造一个只有 payload 有意义的 JWT（不验签，TokenExpiry 只读 exp）。
func fakeJWT(t *testing.T, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp, "iat": exp - 900})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestExportKeepsRefreshToken(t *testing.T) {
	// refresh_token 必须进 accounts.json，否则重启后整池重新登录。
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:t", RefreshToken: "rt-1", Email: "a@b.c", Password: "pw"},
	}, "")
	out := p.Export()
	if len(out) != 1 || out[0].RefreshToken != "rt-1" {
		t.Fatalf("Export 丢了 refresh_token: %+v", out)
	}

	// 重新加载后仍然可用（模拟重启）
	p2 := New(config.Default().Pool, out, "")
	if p2.RefreshToken("a") != "rt-1" {
		t.Fatal("重启后应保留 refresh_token")
	}
}
