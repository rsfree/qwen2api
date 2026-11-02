package server

// 5xx 对客户端的呈现：只给标准英文 + Retry-After，详细原因留在服务端。
//
// 2026-11-02 起因：流水里那条「上游在时间预算内没有任何产物（只发心跳）（预算 5m0s，
// 垫图 0 张）。两种可能：①额度将尽…」被当成会透给调用方。实际上 writeError 已经
// 过 clientError 归一，但这条边界没有测试钉住 —— 顺手把 messages/responses 两条
// 流内错误（当时直接 err.Error()）也拉齐了。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteErrorMasksInternalDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	internal := "上游在时间预算内没有任何产物（只发心跳）（预算 5m0s，垫图 0 张）"
	writeError(rec, http.StatusGatewayTimeout, "upstream_timeout", internal)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码应为 504，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"垫图", "预算", "心跳", "额度"} {
		if strings.Contains(body, leak) {
			t.Fatalf("内部细节 %q 泄露到客户端响应: %s", leak, body)
		}
	}
	if !strings.Contains(body, "upstream timeout") {
		t.Fatalf("应返回标准英文短句，实际 %s", body)
	}
	// 可重试语义要显式：中间网关靠这个头才会自动重试。
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("504 应带 Retry-After: 5，实际 %q", got)
	}
}

func TestWriteErrorKeepsParamHintsFor4xx(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErrorParam(rec, http.StatusBadRequest, "invalid_request_error", "prompt 不能为空（无 image 时必填）", "prompt")

	// 4xx 是调用方自己写错了：具体到字段的中文必须保留，否则没法自助修复。
	if !strings.Contains(rec.Body.String(), "prompt 不能为空") {
		t.Fatalf("4xx 的中文字段提示不该被替换，实际 %s", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("4xx 不该带 Retry-After，实际 %q", got)
	}
}

func TestServiceUnavailableAlsoRetries(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusServiceUnavailable, "service_unavailable", "没有可用账号")
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("503 也应带 Retry-After，实际 %q", got)
	}
}
