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
	sum := sha256.Sum256([]byte(password))
	body, err := json.Marshal(map[string]string{
		"email":    email,
		"password": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/api/v2/auths/signin", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeaders("", c.base+"/")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", transportErr("signin", err)
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
			return "", err
		}
		return "", NewErr(KindAuth, "signin", resp.StatusCode, "",
			"登录成功但响应里没有 token cookie（上游形态可能又变了）", true)
	}

	// 🔴 第二段不能省：signin 签发的 cookie token **只能读、不能写** ——
	// 用它打 /api/v2/chats/new 会得到 Unauthorized（2026-10-28 实测），
	// 而 /api/v2/models/ 却正常。写端点要的是 refresh 换来的 access_token。
	access, err := c.Refresh(ctx, ModeCookie+":"+token)
	if err != nil {
		return "", err
	}
	return ModeBearer + ":" + access, nil
}

// Refresh 用 cookie 凭据换 access_token（Bearer 形态）。
//
// 端点 `GET {auth}/api/v2/auths/refresh`：跨域调用，必须带 x-request-origin
// 且 sec-fetch-site=same-site（见 buildHeadersFor）。
func (c *Client) Refresh(ctx context.Context, cred string) (string, error) {
	base := strings.TrimRight(c.cfg.AuthBaseURL, "/")
	if base == "" {
		base = c.base
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v2/auths/refresh", nil)
	if err != nil {
		return "", err
	}
	req.Header = c.buildHeadersFor(cred, c.base+"/", true)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", transportErr("auths/refresh", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", transportErr("auths/refresh 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "auths/refresh")
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", NewErr(KindUnknown, "auths/refresh", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", NewErr(KindAuth, "auths/refresh", resp.StatusCode, "",
			"未返回 access_token（cookie 可能已失效）", true)
	}
	return out.AccessToken, nil
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
