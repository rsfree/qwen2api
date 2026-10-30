package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// 面板「配置」接口：GET 下发字段表 + 当前值；POST 把 patch 交给 Backend 去热更。
func TestConfigAPIRoundTrip(t *testing.T) {
	b := &stubBackend{cfg: config.Default()}
	b.cfg.APIKeys = []string{"sk-test"}
	b.cfgPath = "/tmp/config.json"
	p := New(b)

	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/panel/api/config", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodGet, "")
	if rec.Code != 200 {
		t.Fatalf("GET 应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Fields   []config.Field `json:"fields"`
		Values   map[string]any `json:"values"`
		Writable bool           `json:"writable"`
		Path     string         `json:"path"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !got.Writable || got.Path != "/tmp/config.json" {
		t.Fatalf("可写状态/路径不对: %+v", got)
	}
	if len(got.Fields) == 0 {
		t.Fatal("应下发字段表")
	}
	// 每个字段都要有值（否则表单渲染成空）
	for _, f := range got.Fields {
		if _, ok := got.Values[f.Key]; !ok {
			t.Fatalf("字段 %s 没有值", f.Key)
		}
	}

	rec = do(http.MethodPost, `{"patch":{"pool.max_rotate":4}}`)
	if rec.Code != 200 {
		t.Fatalf("POST 应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if b.lastPatch["pool.max_rotate"] != float64(4) && b.lastPatch["pool.max_rotate"] != 4 {
		t.Fatalf("patch 没传下去: %+v", b.lastPatch)
	}
}

// 没有配置文件路径时只能看不能改。
func TestConfigAPIReadOnlyWithoutPath(t *testing.T) {
	b := &stubBackend{cfg: config.Default()}
	b.cfg.APIKeys = []string{"sk-test"}
	p := New(b)

	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(`{"patch":{"pool.max_rotate":4}}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("只读时应 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "只能查看") {
		t.Fatalf("错误应说明原因: %s", rec.Body.String())
	}
}
