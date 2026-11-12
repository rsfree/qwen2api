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

// 上游 404 + "Model not found" 必须归到 KindBadInput（调用方的错），
// 而不是 KindUnknown —— 后者会被上层当成"疑似偶发"再换号重试。
//
// 线上证据（2026-11-12）：qwen3.5-plus 下架后 17 分钟 830 次 404。
func TestClassifyModelNotFound(t *testing.T) {
	body := []byte(`{"success":false,"request_id":"x","data":{"code":"Not_Found","details":"Model not found"}}`)
	err := classifyHTTP(404, 404, "chat/completions", body)
	if got := KindOf(err); got != KindBadInput {
		t.Fatalf("模型不存在应归 KindBadInput，得到 %v (%v)", got, err)
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Code != "Model_Not_Found" {
		t.Fatalf("code 应为 Model_Not_Found，得到 %+v", ue)
	}

	// 其它 404（路径错、真 Not_Found）保持原语义，不要误伤。
	other := classifyHTTP(404, 404, "chats/new", []byte(`{"data":{"code":"Not_Found","details":"chat not found"}}`))
	if KindOf(other) != KindUnknown {
		t.Fatalf("会话类 404 应仍是 KindUnknown，得到 %v", KindOf(other))
	}
}
