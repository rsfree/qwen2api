package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// SignIn 账号密码登录，返回**带形态前缀的凭据**（`bearer:<access_token>`）。
//
// 🔴 实测（2026-10-28）三条硬事实：
//  1. 端点仍在**业务域** `chat.qwen.ai/api/v2/auths/signin`；
//     换到 `auth.qwen.ai` 会得到 `Bad_Request: Missing origin`。
//  2. 密码字段是 **sha256hex(明文)**，不是明文。
//  3. 签发的 token **只在 `Set-Cookie`** 里（body 是账号资料，没有 token），
//     且它是 **cookie 形态** —— 当 Bearer 发会 401。这就是本仓要区分形态的原因。
//
// ⚠️ signin 有 IP 级频率墙（同出口连登多号会吃 aliyun_waf 挑战页），
// 所以只在"凭据失效且配了密码"时才调用，不要拿它当轮询。
func (c *Client) SignIn(ctx context.Context, email, password string) (string, error) {
	access, _, err := c.SignInSession(ctx, email, password)
	return access, err
}

// SignInSession 登录并返回**两份**凭据：
//
//	access  = bearer:<jwt>  业务请求用，实测 TTL 只有 900 秒
//	session = cookie:<jwt>  signin 签发的会话，用于 /auths/refresh 换新 access
//
// 🔴 为什么要留 session：access 15 分钟就过期，而 refresh 只需要这个 cookie。
// 以前把它丢掉，等于每次过期都要用密码**从头登录**一次 —— 对 3000 个账号
// 既慢又是风控暴露面（每次 signin 都可能吃 WAF 挑战页）。
func (c *Client) SignInSession(ctx context.Context, email, password string) (access, session string, err error) {
	sum := sha256.Sum256([]byte(password))
	body, err := json.Marshal(map[string]string{
		"email":    email,
		"password": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v2/auths/signin", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header = c.buildHeaders("", c.base+"/")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", transportErr("signin", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// token 在 Set-Cookie，不在 body —— 先取 cookie，再判 body。
	var token string
	for _, ck := range resp.Cookies() {
		if ck.Name == "token" && ck.Value != "" {
			token = ck.Value
			break
		}
	}
	if token == "" {
		if _, err := decodeEnvelope(resp, raw, "signin"); err != nil {
			return "", "", err
		}
		return "", "", NewErr(KindAuth, "signin", resp.StatusCode, "",
			"登录成功但响应里没有 token cookie（上游形态可能又变了）", true)
	}

	// 🔴 第二段不能省：signin 签发的 cookie token **只能读、不能写** ——
	// 用它打 /api/v2/chats/new 会得到 Unauthorized（2026-10-28 实测），
	// 而 /api/v2/models/ 却正常。写端点要的是 refresh 换来的 access_token。
	session = ModeCookie + ":" + token
	at, err := c.Refresh(ctx, session)
	if err != nil {
		return "", "", err
	}
	return ModeBearer + ":" + at, session, nil
}

// Refresh 用 cookie 凭据换 access_token（Bearer 形态）。
//
// 端点 `GET {auth}/api/v2/auths/refresh`：跨域调用，必须带 x-request-origin
// 且 sec-fetch-site=same-site（见 buildHeadersFor）。
func (c *Client) Refresh(ctx context.Context, cred string) (string, error) {
	mode, value := SplitCredential(cred)
	cookie := value
	if mode == ModeCookie && !IsCookieJar(value) {
		cookie = "token=" + value
	}
	access, _, err := c.doRefresh(ctx, cookie)
	return access, err
}

// RefreshWithToken 用 **refresh_token** 换新的 access token。
//
// 🔴 这是"账号保活"的正确姿势（2026-10-30 实测）：
//   - signin 签发的 access token TTL 只有 **900 秒**；
//   - 第一次 refresh 会在 Set-Cookie 里下发 `refresh_token`（约 297B 的 JWT）；
//   - 这个 refresh_token **可以重复使用**（连续 3 次都换到了新 access，不轮换）；
//   - 而 signin 的 token cookie 是一次性的（第二次 refresh 就报"令牌已撤销"）。
//
// 所以只要在登录时把 refresh_token 存下来，之后就不再需要密码 ——
// 既省掉每次过期都重登的开销，也避开 signin 那条最容易吃 WAF 的路径。
func (c *Client) RefreshWithToken(ctx context.Context, refreshToken string) (access, next string, err error) {
	rt := strings.TrimSpace(refreshToken)
	if i := strings.Index(rt, ":"); i > 0 && !strings.Contains(rt, ".") {
		rt = rt[i+1:]
	}
	if rt == "" {
		return "", "", NewErr(KindAuth, "auths/refresh", 0, "", "refresh_token 为空", false)
	}
	return c.doRefresh(ctx, "refresh_token="+rt)
}

// doRefresh 发一次 refresh 请求。cookieHeader 是完整的 Cookie 头内容。
// 返回值 next 是新下发的 refresh_token（首轮才有，后续通常为空）。
func (c *Client) doRefresh(ctx context.Context, cookieHeader string) (access, next string, err error) {
	base := strings.TrimRight(c.cfg.AuthBaseURL, "/")
	if base == "" {
		base = c.base
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v2/auths/refresh", nil)
	if err != nil {
		return "", "", err
	}
	req.Header = c.buildHeadersFor("", c.base+"/", true)
	req.Header.Set("Cookie", cookieHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", transportErr("auths/refresh", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", transportErr("auths/refresh 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "auths/refresh")
	if err != nil {
		return "", "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", "", NewErr(KindUnknown, "auths/refresh", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", "", NewErr(KindAuth, "auths/refresh", resp.StatusCode, "",
			"未返回 access_token（refresh_token 可能已失效）", true)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "refresh_token" && ck.Value != "" {
			next = ck.Value
		}
	}
	return out.AccessToken, next, nil
}

// DetectAuthMode 探测裸 token 的形态：先按 cookie 试，再按 bearer 试。
//
// 为什么必须探测：signin 签发的是 cookie token，网页端的 Bearer 是另一种凭据，
// **两者不可互换**，而且同时发会互相覆盖（Bearer 优先且无效即 401）。
// 已带前缀的凭据直接返回，不产生额外请求。
func (c *Client) DetectAuthMode(ctx context.Context, cred string) (string, error) {
	if mode, value := SplitCredential(cred); mode != "" || value == "" {
		return mode, nil
	}
	_, value := SplitCredential(cred)
	var firstErr error
	for _, mode := range []string{ModeCookie, ModeBearer} {
		if _, err := c.ListModels(ctx, WithMode(mode, value)); err == nil {
			return mode, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = NewErr(KindAuth, "探测凭据形态", 0, "", "两种形态都不可用", true)
	}
	return "", firstErr
}

// NormalizeCredential 把用户输入的凭据整理成带形态的形态（不探测，只按写法推断）。
// 给"启动时来不及探测"的场景用：完整 jar ⇒ cookie，其余 ⇒ 空形态（待探测）。
func NormalizeCredential(cred string) string {
	c := strings.TrimSpace(cred)
	if mode, value := SplitCredential(c); mode != "" {
		return WithMode(mode, value)
	}
	return c
}
