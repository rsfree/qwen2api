package server

// 换票互斥的"拿到锁后复查"：等锁期间别人可能已经续好了。
//
// 为什么值得单独锁一条测试：预热（20s 一轮）和请求路径 relogin 会撞同一个号，
// 加锁只是第一步；如果拿到锁后不复查，就会把刚换来的票再换一次 ——
// 每多换一次就多一次撞 401「令牌已撤销」的机会，而收尾又是最贵的 signin。

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func jwtExpiring(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return "eyJhbGciOiJIUzI1NiJ9." + enc(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + ".sig"
}

func TestRefreshSkipsAlreadyFreshToken(t *testing.T) {
	s, hits := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("token 还新鲜时不该发 refresh 请求（白换一次 = 多一次撞 401 的机会）")
	})

	// 10 分钟后才过期，远大于续期窗口（默认 3 分钟）⇒ 这轮不该动它。
	s.pool.SetToken("a0", "bearer:"+jwtExpiring(t, time.Now().Add(10*time.Minute)))

	if s.refreshTokenOnly(context.Background(), "a0") {
		t.Fatal("没换票却报告 changed，会让 KeepAlive 白写一次盘")
	}
	if n := *hits; n != 0 {
		t.Fatalf("发了 %d 次请求，期望 0 次", n)
	}
}
