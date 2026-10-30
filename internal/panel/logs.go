package panel

// 日志视图的两个数据源：
//
//	/panel/api/requests  请求流水（reqlog 环形缓冲，最多 500 条）
//	/panel/api/runlog    运行日志（stdout/stderr 镜像，最多 1200 行）
//
// 为什么分成两个接口而不是塞进 overview：overview 是 5s 轮询的，
// 把 500 条流水（几十 KB）塞进去等于每 5 秒拖一次全部日志；而日志页
// 完全可以按需拉、按筛选拉。

import (
	"net/http"
	"strings"
)

// handleRequests 请求日志（新 → 旧）。
//
// 查询参数：
//
//	limit    1..500，默认 200
//	q        模糊匹配 路径/模型/账号/来源 IP/请求 ID/错误
//	outcome  ok | failed（空 = 全部）
func (p *Panel) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	outcome := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("outcome")))
	limit := atoiOr(r.URL.Query().Get("limit"), 200)
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}

	all := p.b.Log().Recent(0) // 0 = 整个环形缓冲
	items := make([]any, 0, len(all))
	for _, e := range all {
		ok := e.Status >= 200 && e.Status < 400 && e.Error == ""
		if outcome == "ok" && !ok {
			continue
		}
		if outcome == "failed" && ok {
			continue
		}
		if q != "" && !matchEntry(q, e.Path, e.Model, e.Account, e.ClientIP, e.ID, e.Error, e.UserAgent) {
			continue
		}
		items = append(items, e)
		if len(items) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"total": len(all),
		"limit": limit,
	})
}

func matchEntry(q string, fields ...string) bool {
	for _, f := range fields {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// handleRunLog 运行日志（旧 → 新，终端形态）。
func (p *Panel) handleRunLog(w http.ResponseWriter, r *http.Request) {
	lines := atoiOr(r.URL.Query().Get("lines"), 400)
	if lines <= 0 {
		lines = 400
	}
	if lines > 1200 {
		lines = 1200
	}
	buf := p.b.RunLog()
	if buf == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []any{}, "cached": 0, "enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines":   buf.Lines(lines),
		"cached":  buf.Len(),
		"enabled": true,
	})
}
