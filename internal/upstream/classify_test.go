package upstream

import (
	"errors"
	"testing"
)

// 上游把"账号未激活/被封"塞在 HTTP 400 + code=Bad_Request 里，必须归到账号维度。
//
// 否则：坏号不被停用 + 调用方收到 400 + 一串看不懂的英文原话（2026-10-30 线上实测）。
func TestAccountStateProblemIsAuth(t *testing.T) {
	body := []byte(`{"success":false,"data":{"code":"Bad_Request","details":"Your account is currently pending activation. Please check your email."}}`)
	err := classifyHTTP(400, 400, "chats/new", body)
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("不是 *Error: %v", err)
	}
	if ue.Kind != KindAuth || ue.Code != CodeAccountNotReady {
		t.Fatalf("应归为 KindAuth/%s，得到 %v/%s", CodeAccountNotReady, ue.Kind, ue.Code)
	}
	if !RetrySafe(err) {
		t.Fatal("未受理 ⇒ 换号重试安全")
	}

	// 普通参数错仍然是 BadInput（别把调用方的锅算到账号头上）。
	plain := classifyHTTP(400, 400, "chats/new", []byte(`{"data":{"code":"Bad_Request","details":"model not found"}}`))
	if KindOf(plain) != KindBadInput {
		t.Fatalf("普通 400 应仍是 KindBadInput，得到 %v", KindOf(plain))
	}

	// 业务码路径同样要认。
	biz := classifyBusiness("chat/completions", 200, "Bad_Request", "account suspended")
	if KindOf(biz) != KindAuth {
		t.Fatalf("classifyBusiness 也要认账号状态，得到 %v", KindOf(biz))
	}
}
