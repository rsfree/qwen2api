package server

// 面板调试专用转发。
//
// 为什么需要它：面板可以只用「账号密码」登录（HttpOnly 会话 cookie），
// 此时浏览器里**没有** API Key；而 /v1/* 的闸门只认 Bearer/x-api-key。
// 老版调试台直接 fetch('/v1/chat/completions') ⇒ 必然 401（线上就是这么挂的）。
//
// 正确的边界不是"把 Key 塞回浏览器"，而是：调试请求先过面板会话，再由服务端
// 以内部身份调用前门 handler。所以这些路由**只能挂在面板下**（Panel 已在
// ServeHTTP 顶部挡掉未登录请求），绝不能注册到公开 mux。
//
// 路径改写是必需的：前门 handler 读的是 r.URL.Path（如 /v1/videos/{id} 用
// TrimPrefix 取 id），不是 PathValue。

import (
	"net/http"
	"strconv"
	"strings"
)

// DebugHandler 返回 /panel/api/debug/* 的处理器（鉴权由 Panel 负责）。
func (s *Server) DebugHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/panel/api/debug")

		// 媒体代取：CDN 有防盗链，<img src> 直连会 404（浏览器带的是面板域名的 Referer）。
		if p == "/media" {
			s.handleDebugMedia(w, r)
			return
		}

		r.URL.Path = p
		switch {
		case p == "/v1/chat/completions" && r.Method == http.MethodPost:
			s.handleChat(w, r)
		case p == "/v1/images/generations" && r.Method == http.MethodPost:
			s.handleImages(w, r)
		case p == "/v1/images/edits" && r.Method == http.MethodPost:
			s.handleImageEdits(w, r)
		case p == "/v1/videos/generations" && r.Method == http.MethodPost:
			s.handleVideoCreate(w, r)
		case strings.HasSuffix(p, "/content") && strings.HasPrefix(p, "/v1/videos/"):
			s.handleVideoContent(w, r)
		case strings.HasPrefix(p, "/v1/videos/") && r.Method == http.MethodGet:
			s.handleVideoGet(w, r)
		case p == "/v1/models" && r.Method == http.MethodGet:
			s.handleModels(w, r)
		default:
			writeError(w, http.StatusNotFound, "invalid_request_error", "未知的调试端点: "+p)
		}
	})
}

// handleDebugMedia 代理一个 CDN 媒体直链，只为面板展示服务。
//
// 两道限制，缺一不可：
//   - **主机白名单**（只许 https + *.qwenlm.ai）：否则这个端点就是一台
//     对外开放的 SSRF 跳板（面板能打内网）。
//   - 复用 fetchMedia：它带 Referer/UA，并按魔数校验"下载到的真是媒体"，
//     不会把 226 字节的防盗链 HTML 当图片转给浏览器。
func (s *Server) handleDebugMedia(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "缺少 url")
		return
	}
	if !allowedMediaURL(raw) {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"只允许 https://*.qwenlm.ai 的媒体直链")
		return
	}
	data, err := fetchMedia(r.Context(), raw)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "取回媒体失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", sniffMime(data))
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// allowedMediaURL 只放行 https 且主机属于 qwenlm.ai 的直链。
func allowedMediaURL(raw string) bool {
	if !strings.HasPrefix(raw, "https://") {
		return false
	}
	rest := strings.TrimPrefix(raw, "https://")
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	if i := strings.LastIndexByte(host, '@'); i >= 0 { // 防 https://evil@cdn.qwenlm.ai
		return false
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	return host == "qwenlm.ai" || strings.HasSuffix(host, ".qwenlm.ai")
}
