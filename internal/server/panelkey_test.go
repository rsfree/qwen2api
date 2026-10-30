package server

// 面板密钥的边界 + 多张垫图的请求体上限。
//
// 两件事都在 2026-10-30 被用户实际使用暴露出来：
//  1. 面板改成密钥登录后，那把 Key 绝不能顺手当推理令牌用（面板能删账号）；
//  2. 多张垫图是 base64 内联的，images 端点原来只收 1MB，连一张手机照片都放不下。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 面板密钥只在面板生效：/v1 前门不认它。
func TestPanelKeyNotAcceptedByFrontDoor(t *testing.T) {
	up := newFakeUpstream(t, 0)
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.APIKeys = []string{"sk-front"}
	cfg.Panel.Key = "sk-panel-x"
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	s := New(cfg, pool.New(cfg.Pool, cfg.Accounts, ""), upstream.New(cfg.Upstream), reqlog.New(10), "test")
	h := s.Handler()

	call := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("sk-panel-x"); code != http.StatusUnauthorized {
		t.Fatalf("面板密钥不该能调 /v1（得到 %d）—— 面板能导入/删除账号，权限必须分开", code)
	}
	if code := call("sk-front"); code == http.StatusUnauthorized {
		t.Fatalf("前门令牌应被接受，得到 %d", code)
	}
}

// 多张垫图必须能进门：3 张 1MB 的图 base64 后约 4MB，远超原来 1MB 的上限。
// 如果谁把上限改回去，这条会立刻红。
func TestImagesAcceptsMultiImageBody(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	// data URI 分支不校验魔数（只解码），所以拿零字节当图足够测"能不能进门"。
	one := base64.StdEncoding.EncodeToString(make([]byte, 1<<20))
	imgs := []string{
		"data:image/png;base64," + one,
		"data:image/png;base64," + one,
		"data:image/png;base64," + one,
	}
	body, err := json.Marshal(map[string]any{
		"model": "qwen-image-3.0", "prompt": "把这三张融成一张", "image": imgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 3<<20 {
		t.Fatalf("测试体不够大（%d 字节），起不到守上限的作用", len(body))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body)))

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("多图请求被 400 挡回（body 上限太小？）: %s", rec.Body.String())
	}
	if up.gotSubmit == nil {
		t.Fatalf("请求没到上游（状态 %d）: %s", rec.Code, rec.Body.String())
	}
	files := submitFiles(t, up.gotSubmit)
	if len(files) != 3 {
		t.Fatalf("应带 3 张参考图提交，实际 %d", len(files))
	}
}
