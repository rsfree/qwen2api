package pool

// "只能密码重登"的号：后台要插队修，请求路径要尽量别碰。
//
// 背景（2026-11-02）：401「令牌已撤销」之后，旧代码只是记一条日志就完事 ——
// 号既不会被修，又被预热每 20s 重试一次；真要修只能靠请求撞上，而那一发
// 要多花几秒做 signin（整条链路最贵、最容易吃 WAF 的一步）。

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestAdoptCandidatesPutsNeedLoginFirst(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, nil, "")
	// cold：served 最小（按老规则排最前）；broken：served 很大但票已被撤销。
	p.Add(config.Account{Name: "cold", Token: "bearer:expired", Password: "pw"})
	p.Add(config.Account{Name: "broken", Token: "bearer:expired", Password: "pw"})
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenExp = time.Now().Add(-time.Minute) // 都过期 ⇒ 都算候选
		if a.Name == "broken" {
			a.served = 999
		}
	}
	p.mu.Unlock()
	p.MarkNeedLogin("broken")

	// 只给 1 个名额：不插队的话拿到的是 cold，坏号还得再等一轮。
	names := p.AdoptCandidates(10, 1)
	if len(names) != 1 || names[0] != "broken" {
		t.Fatalf("已确认只能重登的号应插队，实际 %v", names)
	}
}

func TestPickPrefersRefreshableWhenAllStale(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, []config.Account{
		{Name: "needsignin", Token: "bearer:a"},
		{Name: "refreshable", Token: "bearer:b", RefreshToken: "rt-b"},
	}, "")
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenExp = time.Now().Add(-time.Minute) // 全过期：比的是"能不能快速续"
	}
	p.mu.Unlock()

	got, ok := p.Pick()
	if !ok {
		t.Fatal("应能选出账号")
	}
	p.Release(got)
	if got.Name != "refreshable" {
		t.Fatalf("都过期时应优先选有 refresh_token 的号"+
			"（续期几百 ms，而没票的号必须在请求路径上做几秒的 signin），实际选了 %s", got.Name)
	}
}

// 修好之后标记要自动清掉，否则这个号会被永远排在最后。
func TestSetTokenClearsNeedLogin(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	p := New(cfg.Pool, []config.Account{
		{Name: "a", Token: "bearer:old", Email: "a@x.com", Password: "pw"},
	}, "")
	p.MarkNeedLogin("a")

	// 标记期间必须是独立状态：算进"可用"会让人以为这个号能正常服务，
	// 但它其实一直在等后台重登（且 lessPick 把它排在最后）。
	state := ""
	for _, v := range p.Snapshot() {
		if v.Name == "a" {
			state = v.State
		}
	}
	if state != "need_login" {
		t.Fatalf("待重登的号应显示 need_login，实际 %q", state)
	}

	p.SetToken("a", "bearer:"+fakeJWT(t, time.Now().Add(10*time.Minute).Unix()))

	for _, v := range p.Snapshot() {
		if v.Name == "a" && v.NeedLogin {
			t.Fatal("换到新票后 needLogin 必须清掉")
		}
	}
}
