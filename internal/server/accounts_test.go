package server

// 面板账号写操作的测试。
//
// 为什么值得单写一组：导入/删除是**破坏性**的（会写 accounts.json，
// 重启后仍生效），写错一次就是把号弄丢。而这类 bug 在 UI 上表现为
// "导入成功" 但重启后全没了 —— 只在真机上很难复现。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/panel"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// accountsUpstream 假上游：只实现 signin。bad@ 一律密码错。
func accountsUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/signin"):
			var body struct {
				Email string `json:"email"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.HasPrefix(body.Email, "bad@") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"data":{"code":"Unauthorized","details":"密码错误"}}`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "token", Value: "jwt-" + body.Email})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/refresh"):
			// signin 签发的 cookie token 只能读；写端点要的是 refresh 换来的 access_token
			// （见 upstream/signin.go 的注释）。所以假上游两段都要给。
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-ok"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAccountsServer(t *testing.T, apiKeys []string) (*Server, http.Handler, string) {
	t.Helper()
	up := accountsUpstream(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.URL
	cfg.Upstream.AuthBaseURL = up.URL
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.APIKeys = apiKeys
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0", Email: "a0@x.com"}}

	p := pool.New(cfg.Pool, cfg.Accounts, cfg.StateFile)
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	s.SetPanel(panel.New(s))
	return s, s.Handler(), dir
}

func postJSON(h http.Handler, path, body string, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportAccountsValidatesAndPersists(t *testing.T) {
	s, h, dir := newAccountsServer(t, []string{"k"})

	body := `{"accounts":[
		{"name":"b1","email":"b1@x.com","password":"pw"},
		{"name":"b2","email":"b2@x.com","password":"pw"},
		{"name":"b3","email":"bad@x.com","password":"wrong"},
		{"name":"a0","email":"dup@x.com","password":"pw"}]}`

	rec := postJSON(h, "/panel/api/accounts/import", body, "k")
	if rec.Code != 200 {
		t.Fatalf("导入应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK       bool             `json:"ok"`
		Total    int              `json:"total"`
		Imported int              `json:"imported"`
		Skipped  int              `json:"skipped"`
		Results  []pool.AddResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 4 || resp.Imported != 2 || resp.Skipped != 2 {
		t.Fatalf("计数不对: %+v", resp)
	}
	// 失败的两条必须带原因（"跳过"没有原因等于没告诉用户为什么）
	for _, r := range resp.Results {
		if !r.OK && r.Error == "" {
			t.Fatalf("跳过项缺原因: %+v", r)
		}
	}
	if n := s.Pool().Len(); n != 3 {
		t.Fatalf("池内应有 3 个账号（a0 + 2），实际 %d", n)
	}

	// 落盘：accounts.json 必须有 3 条，且 a0 也在（删除/导出是全量语义）
	raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("未写 accounts.json: %v", err)
	}
	var saved []config.Account
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 3 {
		t.Fatalf("文件里应有 3 条，实际 %d", len(saved))
	}
	// 密码必须写进去（否则重启后无法重登，配额与冷却也白存）
	found := false
	for _, a := range saved {
		if a.Name == "b1" {
			found = a.Password == "pw"
		}
	}
	if !found {
		t.Fatalf("密码未落盘: %+v", saved)
	}
}

func TestImportWithoutValidateSkipsSignin(t *testing.T) {
	// 关掉校验时连错密码也进池（用于"我已经确认过凭据"的场景）。
	s, h, dir := newAccountsServer(t, nil)
	rec := postJSON(h, "/panel/api/accounts/import",
		`{"accounts":[{"name":"c1","email":"bad@x.com","password":"wrong"}],"validate":false}`, "")
	if rec.Code != 200 {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	if s.Pool().Len() != 2 {
		t.Fatalf("未校验时应直接入池，池内 %d", s.Pool().Len())
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts.json")); err != nil {
		t.Fatalf("未落盘: %v", err)
	}
}

func TestDeleteAccountRemovesAndPersists(t *testing.T) {
	s, h, dir := newAccountsServer(t, nil)
	postJSON(h, "/panel/api/accounts/import",
		`{"accounts":[{"name":"d1","email":"d1@x.com","password":"pw"}]}`, "")

	rec := postJSON(h, "/panel/api/accounts/delete?name=d1", "", "")
	if rec.Code != 200 {
		t.Fatalf("删除应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if s.Pool().Len() != 1 {
		t.Fatalf("删除后应剩 1 个，实际 %d", s.Pool().Len())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "accounts.json"))
	var saved []config.Account
	_ = json.Unmarshal(raw, &saved)
	for _, a := range saved {
		if a.Name == "d1" {
			t.Fatal("删除后文件里不该还有它")
		}
	}

	// 不存在的号 → 404（而不是假装成功）
	if rec := postJSON(h, "/panel/api/accounts/delete?name=nope", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在的号应 404，得到 %d", rec.Code)
	}
}

func TestImportRequiresKey(t *testing.T) {
	_, h, _ := newAccountsServer(t, []string{"secret"})
	rec := postJSON(h, "/panel/api/accounts/import",
		`{"accounts":[{"name":"x","email":"x@x.com","password":"pw"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，得到 %d", rec.Code)
	}
}

func TestImportRejectsBadPayload(t *testing.T) {
	_, h, _ := newAccountsServer(t, nil)
	if rec := postJSON(h, "/panel/api/accounts/import", `{"accounts":[]}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("空数组应 400，得到 %d", rec.Code)
	}
	if rec := postJSON(h, "/panel/api/accounts/import", `not json`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，得到 %d", rec.Code)
	}
}
