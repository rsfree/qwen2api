package server

// 密码重登（signin）的失败处置。
//
// 两条都是"别让最贵的一步变成常态"：
//   - 失败要退避：预热带按 tick 轮转，不压住就会每 20s 撞一次上游；
//   - 永久失败要停用：密码错的号再试一万次也不会成功，只会白吃 WAF，
//     还一直占着重登队列的最前面（needLogin 是插队的）。

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdoptOneBacksOffAndStopsOnPermanentReject(t *testing.T) {
	s, hits := refreshTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/v2/auths/signin") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"密码错误"}}`))
			return
		}
		http.NotFound(w, r)
	})

	if s.adoptOne(context.Background(), "a0") {
		t.Fatal("401 不该算重登成功")
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("signin 被拒后应只发一次请求，实际 %d 次", n)
	}

	// 401 = 永久失败：必须停用（面板可见、不再被选号），而不是每轮再试。
	state := ""
	for _, v := range s.pool.Snapshot() {
		if v.Name == "a0" {
			state = v.State
		}
	}
	if state != "auth_dead" {
		t.Fatalf("401 后应标成 auth_dead，实际 state=%q", state)
	}

	// 退避期内再调一次：连请求都不该发（省的是上游的 WAF 计数）。
	_ = s.adoptOne(context.Background(), "a0")
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("退避期内不该重试，实际共 %d 次", n)
	}
}
