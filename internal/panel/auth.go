package panel

// 面板登录：**密钥优先**（config.panel.key，留空回落 config.api_keys），
// 账密作为可选后备（只在配了 panel.password 时出现）。
//
// 两种凭据都换同一个签名会话 cookie（HttpOnly，默认 12h）：
//   · 密钥登录不会把明文 Key 留在 localStorage —— 换完 cookie 就丢掉，
//     XSS 也偷不到长期凭据；
//   · 但请求头带 Bearer / x-api-key / ?key= 依然可用（脚本、书签、curl 需要）。
//
// 会话是无状态签名 token（HMAC-SHA256），不落盘、不占内存 —— 单副本够用。
// 多副本要共享 SessionSecret，否则 A 签的会话在 B 上无效（已在 config 注释里写明）。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

const (
	sessionCookie = "qwen2api_panel"
	// 登录失败节流：同 IP 在窗口内失败达到阈值就拒绝，成功即清零。
	// 面板挂在公网（*.1task.cn），没有这道闸等于开放密码爆破。
	loginFailWindow = 10 * time.Minute
	loginFailMax    = 10
)

type failBucket struct {
	count int
	since time.Time
}

// newSecret 取会话签名密钥：配置优先，否则每次启动随机（重启后需重登）。
func newSecret(cfg config.Panel) []byte {
	if s := strings.TrimSpace(cfg.SessionSecret); s != "" {
		return []byte(s)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// rand 失败极罕见；退化成时间派生，至少不 panic。
		return []byte(strconv.FormatInt(time.Now().UnixNano(), 10))
	}
	return b
}

// ---------- 会话签名 ----------

func (p *Panel) issueSession(username string, ttl time.Duration) string {
	payload := username + "|" + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, p.secret)
	mac.Write([]byte(body))
	return body + "." + hex.EncodeToString(mac.Sum(nil))
}

func (p *Panel) sessionValid(token, username string) bool {
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 {
		return false
	}
	body, sigHex := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, p.secret)
	mac.Write([]byte(body))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return false
	}
	// 用户名变了就该失效（改用户名 = 撤销旧会话）
	if parts[0] != username {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return true
}

// ---------- 请求侧判定 ----------

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// nginx 终结 TLS：靠这个头判断，否则 Secure 标记不会加，cookie 可能被明文带出。
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (p *Panel) hasSession(r *http.Request) bool {
	cfg := p.b.Config().Panel
	if !cfg.AuthEnabled() {
		return false
	}
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return false
	}
	return p.sessionValid(ck.Value, cfg.EffectiveUsername())
}

func (p *Panel) apiKeyOK(r *http.Request, keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	token := ""
	if v := r.Header.Get("Authorization"); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			token = strings.TrimSpace(v[7:])
		} else {
			token = strings.TrimSpace(v)
		}
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	return matchKey(token, keys)
}

// keyStrings 面板认的密钥：自定义面板密钥优先，其次前门令牌。
//
// 为什么要"回落前门令牌"：单 Key 部署（只配 config.api_keys）的用户升级后
// 不该先改配置才能进面板。两把都认，撤销 api_key 时面板也一起失效。
// 反过来不成立：面板密钥只在面板生效，/v1 不认（见 config.Panel.Key 注释）。
func (p *Panel) keyStrings() []string {
	out := make([]string, 0, 4)
	if k := p.b.Config().Panel.PanelKey(); k != "" {
		out = append(out, k)
	}
	for _, k := range p.b.APIKeys() {
		if k.Key != "" {
			out = append(out, k.Key)
		}
	}
	return out
}

// matchKey 常量时间比较，避免"逐字节比较 + 提前返回"泄漏前缀。
func matchKey(token string, keys []string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// authMethod 返回本次请求的凭据来源：key / session / open / ""（未通过）。
func (p *Panel) authMethod(r *http.Request) string {
	keys := p.keyStrings()
	if p.apiKeyOK(r, keys) {
		return "key"
	}
	if p.hasSession(r) {
		return "session"
	}
	if !p.b.Config().Panel.AuthEnabled() && len(keys) == 0 {
		return "open"
	}
	return ""
}

// ---------- 处理函数 ----------

// handleSession 前端启动时问一句"我该怎么证明身份"。
func (p *Panel) handleSession(w http.ResponseWriter, r *http.Request) {
	cfg := p.b.Config().Panel
	method := p.authMethod(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"need_auth":        cfg.AuthEnabled() || len(p.keyStrings()) > 0,
		"authed":           method != "",
		"method":           method,
		"username":         cfg.EffectiveUsername(),
		// key_only 面板默认就是密钥登录；只有配了密码时才多一个"账密登录"入口。
		"key_only":         !cfg.PasswordEnabled(),
		"password_enabled": cfg.PasswordEnabled(),
		"panel_key":        cfg.PanelKey() != "",
		"session_hr":       cfg.EffectiveSessionHours(),
	})
}

func (p *Panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	cfg := p.b.Config().Panel
	keys := p.keyStrings()
	if !cfg.AuthEnabled() && len(keys) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "未启用面板登录（config.panel.key / panel.password / api_keys 都为空）"})
		return
	}
	ip := clientIP(r)
	if p.throttled(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "失败次数过多，请 10 分钟后再试（或改 config.panel.password 后重启）",
		})
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败"})
		return
	}

	// ① 密钥登录（默认形态）：面板密钥或前门令牌都认。
	//    换到 HttpOnly 会话后就丢掉明文 —— 前端也不再往 localStorage 里存。
	if k := strings.TrimSpace(body.Key); k != "" {
		if !matchKey(k, keys) {
			p.recordFail(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "密钥无效"})
			return
		}
		p.clearFail(ip)
		p.issueSessionCookie(w, r, cfg)
		return
	}

	// ② 账密登录（可选后备）：只有配了密码才认，绝不"空密码即通过"。
	if !cfg.PasswordEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "未启用账密登录（config.panel.password 为空），请用密钥登录"})
		return
	}
	userOK := subtle.ConstantTimeCompare(
		[]byte(strings.TrimSpace(body.Username)), []byte(cfg.EffectiveUsername())) == 1
	passOK := config.VerifyPanelPassword(cfg, body.Password)
	if !userOK || !passOK {
		p.recordFail(ip)
		// 不区分"用户名错"还是"密码错"：区分等于送攻击者一个用户名枚举接口。
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	p.clearFail(ip)
	p.issueSessionCookie(w, r, cfg)
}

// issueSessionCookie 下发 HttpOnly 会话 cookie（密钥登录 / 账密登录共用）。
//
// 密钥登录也走会话，而不是"每个请求都带明文 Key"：
//   - 浏览器侧：明文只在登录那一次出现，之后由 HttpOnly cookie 代理
//     （XSS 拿不到；localStorage 里也不留长期凭据）；
//   - 服务端：撤销密钥只需改配置重启，会话的 TTL 到期即止（12h 后可再收紧）。
func (p *Panel) issueSessionCookie(w http.ResponseWriter, r *http.Request, cfg config.Panel) {
	ttl := time.Duration(cfg.EffectiveSessionHours()) * time.Hour
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    p.issueSession(cfg.EffectiveUsername(), ttl),
		Path:     "/",
		HttpOnly: true, // JS 读不到：XSS 也偷不走
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "method": "session", "expires_in_s": int(ttl.Seconds()),
	})
}

func (p *Panel) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 失败节流 ----------

func (p *Panel) throttled(ip string) bool {
	p.limMu.Lock()
	defer p.limMu.Unlock()
	b := p.fails[ip]
	if b == nil {
		return false
	}
	if time.Since(b.since) > loginFailWindow {
		delete(p.fails, ip)
		return false
	}
	return b.count >= loginFailMax
}

func (p *Panel) recordFail(ip string) {
	p.limMu.Lock()
	defer p.limMu.Unlock()
	b := p.fails[ip]
	if b == nil || time.Since(b.since) > loginFailWindow {
		p.fails[ip] = &failBucket{count: 1, since: time.Now()}
		return
	}
	b.count++
}

func (p *Panel) clearFail(ip string) {
	p.limMu.Lock()
	delete(p.fails, ip)
	p.limMu.Unlock()
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
