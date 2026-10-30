package upstream

// 按账号分流的代理路由。
//
// 设计要点：**不改任何方法签名**。账号身份通过 context 传递
// （`WithAccount`），由 RoundTripper 在真正发包时读取 —— 因为上游调用点有十几处，
// 每处都加一个参数既啰嗦又容易漏（漏一处的后果是"那个请求走了真实 IP"，很难发现）。
//
// 命中规则：
//   - proxy.enabled=false        ⇒ 直连（与历史行为一致）
//   - 有账号上下文 + 模板         ⇒ 该账号专属出口（账号名替换 {account}）
//   - 无账号上下文               ⇒ 按 direct_when_missing 决定直连还是报错
//
// 每个账号一条 transport（连接复用 + 出口稳定）。transport 是按账号缓存的，
// 不随 token 变化 —— 换 token 不该换出口。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

type accountCtxKey struct{}

// WithAccount 给请求上下文打上账号标记。
func WithAccount(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, name)
}

// AccountOf 取出账号标记（空表示没有）。
func AccountOf(ctx context.Context) string {
	if v, ok := ctx.Value(accountCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// proxyRouter 按账号选 transport。
type proxyRouter struct {
	base  *http.Transport
	cfg   config.Proxy
	mu    sync.Mutex
	cache map[string]*http.Transport
}

func newProxyRouter(base *http.Transport, cfg config.Proxy) http.RoundTripper {
	return &proxyRouter{base: base, cfg: cfg, cache: map[string]*http.Transport{}}
}

func (r *proxyRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, err := r.transportFor(AccountOf(req.Context()))
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

func (r *proxyRouter) transportFor(account string) (*http.Transport, error) {
	if !r.cfg.Enabled || strings.TrimSpace(r.cfg.URLTemplate) == "" {
		return r.base, nil
	}
	if account == "" {
		// 公共端点（如未登录探测）没有账号归属：直连还是拒绝，是策略问题。
		if r.cfg.DirectWhenMissing == nil || *r.cfg.DirectWhenMissing {
			return r.base, nil
		}
		return nil, errors.New("代理已启用且禁止直连，但本次请求缺少账号上下文")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if tr, ok := r.cache[account]; ok {
		return tr, nil
	}
	tr, err := r.buildFor(account)
	if err != nil {
		return nil, err
	}
	r.cache[account] = tr
	return tr, nil
}

// buildFor 给单个账号建 transport。
func (r *proxyRouter) buildFor(account string) (*http.Transport, error) {
	raw := expandProxyURL(r.cfg.URLTemplate, account, r.cfg.Password)
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 解析失败（检查 url_template）: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
	default:
		// 只支持 SOCKS5：HTTP 代理在这里没有意义（我们要的是"换出口 IP"，
		// 而 HTTP CONNECT 代理通常仍带 X-Forwarded-For，起不到隔离作用）。
		return nil, fmt.Errorf("代理只支持 socks5/socks5h，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("代理 URL 缺少 host:port")
	}
	user, pass := "", ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	proxyAddr := u.Host

	tr := r.base.Clone()
	// 关键：必须清掉环境代理，否则 socks 隧道又被 HTTP_PROXY 劫一层。
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return socks5Dial(ctx, proxyAddr, user, pass, addr)
	}
	return tr, nil
}

// expandProxyURL 替换 {account} / {password}。
//
// 只替换这两个占位符：模板里可能出现 `{` 的其它用法（比如 JSON 里的花括号），
// 用正则做通用替换容易误伤。
func expandProxyURL(tpl, account, password string) string {
	// PathEscape 而不是 QueryEscape：后者把空格编成 "+"，在 userinfo 里是字面加号，
	// 会变成"密码多了一个加号"这种极难查的登录失败。
	out := strings.ReplaceAll(tpl, "{account}", url.PathEscape(account))
	out = strings.ReplaceAll(out, "{password}", url.PathEscape(password))
	return out
}
