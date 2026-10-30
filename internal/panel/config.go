package panel

import (
	"encoding/json"
	"net/http"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// handleConfigGet 返回「配置」页要的表单：字段定义 + 当前值 + 能否写。
//
// 字段清单来自 config.Fields（白名单）—— 面板只能改这里登记过的项。
// 其余配置（listen / accounts / api_keys / panel / upstream.base_url）
// 要么是启动期资源、要么写错会把服务锁死，只让改文件 + 重启。
func (p *Panel) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := p.b.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"fields": config.Fields,
		// values 用「配置快照 → JSON → 按点分路径取值」，与 Fields 的 key 一一对应。
		"values": config.FieldValues(cfg),
		// writable=false 时前端把表单置灰（例如纯环境变量部署没给 -config）。
		"writable": p.b.ConfigPath() != "",
		"path":     p.b.ConfigPath(),
		// 只读信息：让人知道"改了哪些要重启"。
		"restart_only": []string{
			"listen", "state_file", "api_keys", "panel.*",
			"upstream.base_url", "accounts",
		},
	})
}

// handleConfigSave 保存表单并热更新。
//
// 请求体：{"patch": {"pool.max_rotate": 3, ...}} —— 只提交**改动过的**字段，
// 没提交的项不动（避免把用户没碰的配置写回旧值）。
func (p *Panel) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	if p.b.ConfigPath() == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "启动时没指定配置文件（-config），面板只能查看不能修改",
		})
		return
	}
	var body struct {
		Patch map[string]any `json:"patch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败：" + err.Error()})
		return
	}
	if len(body.Patch) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有要保存的改动"})
		return
	}

	applied, needRestart, err := p.b.UpdateConfig(body.Patch)
	if needRestart == nil {
		needRestart = []string{} // JSON 里给 []，别给 null（前端少一次判空）
	}
	if err != nil {
		// 校验失败是 400（用户改错了）；写盘失败也是 500 更合适，但这里不细分 ——
		// 文案里已经说明是哪一步。
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"applied":      applied,
		"need_restart": needRestart,
		"values":       config.FieldValues(p.b.Config()),
	})
}
