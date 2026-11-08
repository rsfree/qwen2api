package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthErrorEnvelopeCarriesCode 401 的信封必须带 code。
//
// 🔴 为什么单独钉这条：鉴权是**横切**错误，由 wrap 统一产生，服务 chat / images /
// messages / videos 全部端点。它一度是裸两键 `{message,type}`（writeError 的默认形态），
// 于是方舟客户端按 `error.code` 分支时拿到空值 —— 而同一网关的业务错误（缺 content、
// 场景冲突）都是四键齐备的。同一個 error 对象两种形状，客户端只能写两套判断。
//
// 注意断言的是**跨端点一致**：不是「视频端点特殊照顾」，而是所有端点同一形状。
func TestAuthErrorEnvelopeCarriesCode(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, []string{"secret-key"})

	probes := []struct{ method, path string }{
		{http.MethodPost, videoTaskPath},
		{http.MethodGet, videoTaskPath},
		{http.MethodGet, videoTaskPath + "/cgt-x"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/images/generations"},
	}
	for _, p := range probes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(p.method, p.path, strings.NewReader(`{}`)))

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 无 key 应 401，得到 %d: %s", p.method, p.path, rec.Code, rec.Body.String())
		}
		var body struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s 回包不是 JSON: %v（%s）", p.method, p.path, err, rec.Body.String())
		}
		if body.Error["code"] != errCodeAuth {
			t.Fatalf("%s %s 的 code 应为 %q，实际 %v（%s）",
				p.method, p.path, errCodeAuth, body.Error["code"], rec.Body.String())
		}
		if body.Error["type"] != "authentication_error" {
			t.Fatalf("%s %s 的 type 应为 authentication_error，实际 %v",
				p.method, p.path, body.Error["type"])
		}
		if msg, _ := body.Error["message"].(string); msg == "" {
			t.Fatalf("%s %s 缺 message", p.method, p.path)
		}
		// param 省略：鉴权失败指不到任何请求体字段，给空串比不给更糟
		// （客户端会去 error.param 里找问题，而问题在 Authorization 头）。
		if _, ok := body.Error["param"]; ok {
			t.Fatalf("%s %s 不该带 param: %s", p.method, p.path, rec.Body.String())
		}
	}
}
