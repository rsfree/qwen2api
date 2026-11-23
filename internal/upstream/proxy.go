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

	// accountProxy "账号名 → 专属出口 URL"解析器（main 在 pool 就绪后注入）。
	// nil = 未注入（测试场景），只走 url_template / 直连。
	accountProxyMu sync.RWMutex
	accountProxy   func(string) string
}

func newProxyRouter(base *http.Transport, cfg config.Proxy) http.RoundTripper {
	return &proxyRouter{base: base, cfg: cfg, cache: map[string]*http.Transport{}}
}

// SetAccountProxyResolver 注入解析器。注入/换人都不清 cache ——
// cache 键里带 URL（见 transportFor），换 URL 自然是新键，旧键随 LRU 无所谓。
func (r *proxyRouter) SetAccountProxyResolver(f func(string) string) {
	r.accountProxyMu.Lock()
	r.accountProxy = f
	r.accountProxyMu.Unlock()
}

func (r *proxyRouter) resolveAccountProxy(account string) string {
	r.accountProxyMu.RLock()
	f := r.accountProxy
	r.accountProxyMu.RUnlock()
	if f == nil || account == "" {
		return ""
	}
	return strings.TrimSpace(f(account))
}

func (r *proxyRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	// 内网/回环目标直连：典型是本地兜底通道（browser bridge 监听在 docker
	// 网桥网关 IP 上）。socks5 出口是给上游用的，它到不了私有网段，
	// 硬走代理只会得到 "CONNECT 被拒（网络不可达）"。
	if isDirectHost(req.URL.Hostname()) {
		if r.cfg.Enabled && r.cfg.DirectWhenMissing != nil && !*r.cfg.DirectWhenMissing {
			// 运维显式禁止直连（"绝不允许真实 IP 出现"）时，内网目标也不放行，
			// 否则这条旁路会让该承诺失效。
			return nil, fmt.Errorf("目标 %s 属于内网，但配置禁止直连", req.URL.Hostname())
		}
		return r.base.RoundTrip(req)
	}
	tr, err := r.transportFor(AccountOf(req.Context()))
	if err != nil {
		return nil, err
	}
	return tr.RoundTrip(req)
}

// isDirectHost 目标主机是否属于"不该走出口代理"的本地/内网地址。
//
// 只认**字面 IP** 的回环与 RFC1918/ULA 私有网段；主机名（含 localhost）
// 一律返回 false —— 主机名要继续交给代理做远端解析（socks5h 语义），
// 而且宁可漏判（走代理后失败，报错明确）也不能误判（把本该走代理的
// 上游请求暴露成真实 IP）。
func isDirectHost(host string) bool {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	// 注意：auth.qwen.ai 不能直连 —— 实测直连返回 405 + HTML 错误页（该域对机房
	// IP 有防护），只能继续走 socks5 出口。它的问题是延迟（TTFB 4.4s），
	// 已通过放宽 refresh 超时预算解决（见 signin.doRefresh）。
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

func (r *proxyRouter) transportFor(account string) (*http.Transport, error) {
	// 1) 账号专属 URL —— **即使 upstream.proxy.enabled=false 也生效**：
	//    URL 写在账号身上，就是运维明确要"这个号走这个出口"，
	//    拿全局开关去否决它反而违背字段的本意。
	//    典型场景：signin 的 IP 频率墙把主出口打花了，只给新导入的几个号
	//    指一条住宅出口，主流量继续直连。
	if account != "" {
		if raw := r.resolveAccountProxy(account); raw != "" {
			return r.transportForURL(account, raw)
		}
	}
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
	return r.transportForURL(account, expandProxyURL(r.cfg.URLTemplate, account, r.cfg.Password))
}

// transportForURL 按键（账号 + 完整 URL）取缓存的 transport，
// 没有则现建。键里带 URL：账号的 URL 变了（面板改代理）不会命中旧连接。
func (r *proxyRouter) transportForURL(account, raw string) (*http.Transport, error) {
	key := account + "\x00" + raw
	r.mu.Lock()
	defer r.mu.Unlock()
	if tr, ok := r.cache[key]; ok {
		return tr, nil
	}
	tr, err := buildForURL(r.base, raw)
	if err != nil {
		return nil, err
	}
	r.cache[key] = tr
	return tr, nil
}

// buildForURL 给单个出口建 transport。
//
// 支持两类出口：
//   - socks5 / socks5h：换出口 IP + 远端 DNS；
//   - http / https：CONNECT 隧道。TLS 是端到端的（CONNECT 只转发 TCP 流），
//     换出口 IP 的目的完全达成；CONNECT 本身也不加 X-Forwarded-For
//     （那是代理后端应用自己加的头）。住宅 HTTP 代理是最容易买到的
//     出口形态，不支持它等于把这类资源拒之门外。
func buildForURL(base *http.Transport, raw string) (*http.Transport, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理 URL 解析失败（检查 url_template / accounts[].proxy）: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
	case "http", "https":
	default:
		// 不支持的 scheme 直接报错：静默直连会把"以为走了出口"的请求
		// 变成真实 IP 直连 —— 这是本文件最要防的事故。
		return nil, fmt.Errorf("代理只支持 socks5/socks5h/http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("代理 URL 缺少 host:port")
	}
	switch u.Scheme {
	case "http", "https":
		tr := base.Clone()
		// ProxyFromEnvironment 必须被显式覆盖，否则环境里的 HTTP_PROXY
		// 会跟这里指定的出口叠一层。
		tr.Proxy = http.ProxyURL(u)
		return tr, nil
	}
	user, pass := "", ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	proxyAddr := u.Host

	tr := base.Clone()
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
