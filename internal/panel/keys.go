package panel

// 前门令牌（调用模型用的 API Key）的读取与管理。
//
// 为什么面板要能看明文：这是用户最常问的一句话"token 是啥"。
// 之前配置页只写「API Key 已启用」，不给值也不给用法 —— 结果就是每个人都要
// 回头翻 config.json 甚至翻部署脚本。面板本身就是管理员入口（能导入/删除账号、
// 能看到全部邮箱），在这里回显 key 不引入新的信任边界；反倒是不回显，
// 逼着大家把 key 抄在聊天记录里，泄漏面更大。
//
// 前端默认按掩码展示，点「显示」才展开 —— 防的是肩膀偷看和录屏，不是防管理员。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// handleKeys 列出生效的 key + 接入信息。
func (p *Panel) handleKeys(w http.ResponseWriter, r *http.Request) {
	keys := p.b.APIKeys()
	items := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		items = append(items, map[string]any{
			"key":     k.Key,
			"mask":    config.MaskAPIKey(k.Key),
			"note":    k.Note,
			"created": k.Created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":          items,
		"managed":       p.b.APIKeysManaged(),
		"path":          p.b.APIKeysPath(),
		"base_url":      publicBaseURL(r),
		"auth_required": len(items) > 0,
		"mask_hint":     "面板里显示的是完整 key；默认掩码只是防窥屏。",
	})
}

// createKey 生成一把新 key（首次会领养 config 里的旧 key，见 server.AddAPIKey）。
func (p *Panel) createKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	k, err := p.b.AddAPIKey(body.Note)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"key":  k.Key,
		"mask": config.MaskAPIKey(k.Key),
		"note": k.Note,
	})
}

func (p *Panel) deleteKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败"})
		return
	}
	if err := p.b.DeleteAPIKey(body.Key); err != nil {
		// 400 而不是 500：绝大多数是"删最后一把"或"已经被别人撤销了"，
		// 这是用户输入问题，前端要能原样展示那句话。
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publicBaseURL 推断"客户端该填的 Base URL"。
//
// 面板可能挂在 nginx 后面（TLS 在边缘终结），所以先看 X-Forwarded-*，
// 再退回 TLS 与 Host。返回的是根地址，/v1 由前端拼（配置页要同时展示两者）。
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if i := strings.IndexByte(host, ','); i > 0 {
		host = strings.TrimSpace(host[:i])
	}
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}
