package server

// 401 之后的恢复时延（2026-11-13 线上事故）。
//
// 现场：refresh_token 被上游撤销（`auths/refresh` 401「令牌已撤销」）后，账号
// 在日志里只留一条失败记录，随后整整 5 分钟没有任何修复动作 —— 因为 401 的
// 时间戳被写进了 refreshRetryBackoff，而密码重登（adoptOne）走的是同一张退避表。
//
// 401 是**确定性**失败：票已经清空，不存在「再撞同一张死票」的风险，退避在这里
// 没有任何防护价值，只会把唯一有效的修复动作推迟满 5 分钟。
//
// 这两条测试钉住两件事：
//   1. 401 清票后必须**立刻**允许密码重登（下一个 tick，20s）；
//   2. 请求路径撞到 401 也必须当场丢掉死票，不能让后续每次重试都白付一次
//      refresh 往返。

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefresh401UnblocksRelogin(t *testing.T) {
	s, hits := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/auths/signin"):
			http.SetCookie(w, &http.Cookie{Name: "token", Value: "session-jwt"})
			_, _ = w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/auths/refresh"):
			// 旧票（rt-old）已被撤销；signin 之后用 token cookie 换 access 才是新票。
			if strings.Contains(r.Header.Get("Cookie"), "refresh_token=rt-old") {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"令牌已撤销，请重新登录。"}}`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "rt-new"})
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-new"}}`))
		default:
			http.NotFound(w, r)
		}
	})

	if s.refreshTokenOnly(context.Background(), "a0") {
		t.Fatal("401 不该算续期成功")
	}
	// 这条断言就是本次修复的核心：票已清空，就不该再压 5 分钟退避。
	if !s.refreshDue("a0", time.Now()) {
		t.Fatal("401 清票后必须立刻允许密码重登，否则账号白停 5 分钟")
	}
	if !s.adoptOne(context.Background(), "a0") {
		t.Fatalf("下一个 tick 就该重登成功，实际失败（上游命中 %d 次）", atomic.LoadInt32(hits))
	}
	if got := s.pool.RefreshToken("a0"); got != "rt-new" {
		t.Fatalf("重登后应换上新的 refresh_token，实际 %q", got)
	}
	if s.pool.Snapshot()[0].NeedLogin {
		t.Fatal("换到新票后不该再挂 need_login，否则选号一直把它排最后")
	}
}

func TestReloginDropsRevokedTicket(t *testing.T) {
	s, _ := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auths/signin") {
			w.WriteHeader(http.StatusBadGateway) // 登录暂时不可用：模拟 WAF/网关抖动
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"令牌已撤销，请重新登录。"}}`))
	})

	acct := s.pool.Get("a0")
	if acct == nil {
		t.Fatal("账号应在池里")
	}
	if s.relogin(context.Background(), acct) {
		t.Fatal("signin 失败时不该报续期成功")
	}
	if got := s.pool.RefreshToken("a0"); got != "" {
		t.Fatalf("请求路径撞到 401 也必须丢掉死票，实际仍留着 %q —— "+
			"此后每次重试都要先白付一次 refresh 往返（几百 ms）", got)
	}
}
