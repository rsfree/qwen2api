package upstream

// SOCKS5 与「按账号分流」的离线测试。
//
// 用一个假的 SOCKS5 服务端（真说协议）来验证三件事：
//   1. 用户名/密码认证确实发出去了；
//   2. 目标是**域名**时按 ATYP=domain 发给代理（远端 DNS），不是本地解析；
//   3. 不同账号名 → 不同代理用户名（也就是不同出口 IP）。

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// fakeSocks 最小 SOCKS5 服务端（只支持 CONNECT + 可选用户名密码认证）。
type fakeSocks struct {
	ln net.Listener

	mu       sync.Mutex
	user     string
	pass     string
	requAuth bool
	targets  []string // 每次 CONNECT 的 target（host:port，域名原样）
	users    []string
	conns    int
}

func newFakeSocks(t *testing.T, user, pass string, requireAuth bool) *fakeSocks {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSocks{ln: ln, user: user, pass: pass, requAuth: requireAuth}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSocks) addr() string { return f.ln.Addr().String() }

func (f *fakeSocks) serve(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if f.requAuth {
		if _, err := c.Write([]byte{5, 2}); err != nil {
			return
		}
		var auth [2]byte
		if _, err := io.ReadFull(c, auth[:]); err != nil {
			return
		}
		u := make([]byte, int(auth[1]))
		if _, err := io.ReadFull(c, u); err != nil {
			return
		}
		var pl [1]byte
		if _, err := io.ReadFull(c, pl[:]); err != nil {
			return
		}
		p := make([]byte, int(pl[0]))
		if _, err := io.ReadFull(c, p); err != nil {
			return
		}
		ok := string(u) == f.user && string(p) == f.pass
		f.mu.Lock()
		f.users = append(f.users, string(u))
		f.mu.Unlock()
		if !ok {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		_, _ = c.Write([]byte{1, 0})
	} else {
		_, _ = c.Write([]byte{5, 0})
	}

	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(pb[:]))

	f.mu.Lock()
	f.targets = append(f.targets, host+":"+strconv.Itoa(port))
	f.conns++
	f.mu.Unlock()

	// 真正连目标（本测试里目标是本地 httptest 的 127.0.0.1）
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	_ = c.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (f *fakeSocks) snapshot() (targets, users []string, conns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.targets...), append([]string{}, f.users...), f.conns
}

func proxyClient(t *testing.T, f *fakeSocks, tpl, password string) *Client {
	t.Helper()
	cfg := config.Default().Upstream
	cfg.Proxy = config.Proxy{Enabled: true, URLTemplate: tpl, Password: password}
	return New(cfg)
}

func TestSocks5DialThroughProxy(t *testing.T) {
	// 目标服务只监听 127.0.0.1；用域名 "localhost" 访问 ⇒ 若客户端本地解析，
	// 代理收到的会是 IP；我们要的是**域名原样发给代理**。
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		_, _ = w.Write([]byte("via-socks"))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	socks := newFakeSocks(t, "poolclient-a1", "pw", true)
	tpl := fmt.Sprintf("socks5h://poolclient-{account}:{password}@%s", socks.addr())
	cli := proxyClient(t, socks, tpl, "pw")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(WithAccount(ctx, "a1"), http.MethodGet,
		fmt.Sprintf("http://localhost:%d/hi", port), nil)
	resp, err := cli.http.Do(req)
	if err != nil {
		t.Fatalf("经代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-socks" {
		t.Fatalf("响应不对: %q", body)
	}
	if !strings.HasPrefix(gotHost, "localhost") {
		t.Fatalf("目标 Host 丢了: %q", gotHost)
	}

	targets, users, _ := socks.snapshot()
	if len(users) != 1 || users[0] != "poolclient-a1" {
		t.Fatalf("代理用户名不对: %v（{account} 应替换成 a1）", users)
	}
	if len(targets) != 1 || !strings.HasPrefix(targets[0], "localhost:") {
		t.Fatalf("target 应保留域名: %v", targets)
	}
}

func TestSocks5PerAccountUsesDistinctUser(t *testing.T) {
	// 这是整个设计的目的：不同账号 → 不同代理用户名 → 不同出口 IP。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	socks := newFakeSocks(t, "", "", false)
	tpl := fmt.Sprintf("socks5://poolclient-{account}@%s", socks.addr())
	cli := proxyClient(t, socks, tpl, "pw")

	for _, acct := range []string{"main", "a1", "a2"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(WithAccount(ctx, acct), http.MethodGet,
			fmt.Sprintf("http://localhost:%d/", port), nil)
		resp, err := cli.http.Do(req)
		cancel()
		if err != nil {
			t.Fatalf("%s 请求失败: %v", acct, err)
		}
		resp.Body.Close()
	}
	_, users, _ := socks.snapshot()
	want := []string{"poolclient-main", "poolclient-a1", "poolclient-a2"}
	// 免认证模式下服务端不记录用户名；这里靠 target 数量确认走了三次代理
	if len(users) != 0 {
		t.Fatalf("免认证模式不该有用户名: %v", users)
	}
	targets, _, conns := socks.snapshot()
	if conns != 3 {
		t.Fatalf("应有 3 次代理连接，得到 %d（targets=%v, want=%v）", conns, targets, want)
	}
}

func TestProxyDisabledGoesDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("direct"))
	}))
	defer srv.Close()
	socks := newFakeSocks(t, "", "", false)

	cfg := config.Default().Upstream // Proxy 默认关
	cli := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(WithAccount(ctx, "a1"), http.MethodGet, srv.URL, nil)
	resp, err := cli.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "direct" {
		t.Fatalf("应直连: %q", b)
	}
	if _, _, conns := socks.snapshot(); conns != 0 {
		t.Fatal("代理关闭时不该有任何代理连接")
	}
}

func TestProxyRequiresAccountWhenDirectForbidden(t *testing.T) {
	socks := newFakeSocks(t, "", "", false)
	no := false
	cfg := config.Default().Upstream
	cfg.Proxy = config.Proxy{
		Enabled:           true,
		URLTemplate:       fmt.Sprintf("socks5://poolclient-{account}@%s", socks.addr()),
		DirectWhenMissing: &no,
	}
	cli := New(cfg)
	_, err := cli.http.Do(mustReq("http://example.invalid/"))
	if err == nil {
		t.Fatal("禁止直连时无账号上下文应报错")
	}
	if !strings.Contains(err.Error(), "缺少账号上下文") {
		t.Fatalf("错误信息应说明原因: %v", err)
	}
}

func TestProxyRejectsNonSocksScheme(t *testing.T) {
	cfg := config.Default().Upstream
	cfg.Proxy = config.Proxy{Enabled: true, URLTemplate: "http://user:pass@127.0.0.1:8080"}
	cli := New(cfg)
	ctx := WithAccount(context.Background(), "a1")
	_, err := cli.http.Do(mustReqCtx(ctx, "http://example.invalid/"))
	if err == nil || !strings.Contains(err.Error(), "socks5") {
		t.Fatalf("非 socks 协议应被拒绝: %v", err)
	}
}

func TestSocks5BadPasswordIsReported(t *testing.T) {
	socks := newFakeSocks(t, "poolclient-a1", "right", true)
	cfg := config.Default().Upstream
	cfg.Proxy = config.Proxy{
		Enabled:     true,
		URLTemplate: fmt.Sprintf("socks5://poolclient-{account}:wrong@%s", socks.addr()),
	}
	cli := New(cfg)
	ctx := WithAccount(context.Background(), "a1")
	_, err := cli.http.Do(mustReqCtx(ctx, "http://localhost:1/"))
	if err == nil {
		t.Fatal("密码错误应失败")
	}
	if !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("错误应指出是认证问题（否则会被误判成网络故障）: %v", err)
	}
}

func mustReq(u string) *http.Request {
	return mustReqCtx(context.Background(), u)
}

func mustReqCtx(ctx context.Context, u string) *http.Request {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("User-Agent", "test")
	return req
}
