package upstream

// 出口 IP 探测。
//
// 为什么必须"实测"而不是"推导"：代理是 socks5h://poolclient-{account}@pool.livetest.cn:2090，
// 所有账号共用同一个 **host**，但出口 IP 由运营商侧按账号分配（甚至同一账号会漂）。
// 上游的风控/封号恰恰按 IP 画像，所以排障时必须知道"这一次是从哪个 IP 出去的"。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultEgressIPURL 默认探测地址（纯文本返回来源 IP）。
const DefaultEgressIPURL = "https://api.ipify.org"

// EgressIP 从该账号的出口发一次请求，返回对面看到的来源 IP。
//
// 🔴 这是**后台辅助动作**：8 秒硬超时 + 不重试。它只服务面板展示与排障，
// 绝不允许拖慢主链路（调用方永远是 goroutine，见 server/egress.go）。
func (c *Client) EgressIP(ctx context.Context, account string) (string, error) {
	raw := strings.TrimSpace(c.cfg.EgressIPURL)
	if raw == "-" {
		return "", errors.New("出口 IP 探测已关闭（upstream.egress_ip_url = \"-\"）")
	}
	if raw == "" {
		raw = DefaultEgressIPURL
	}
	ctx, cancel := context.WithTimeout(WithAccount(ctx, account), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	// 少数探测站会按 UA 拒绝空 UA；带上网关的浏览器 UA 最稳。
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("探测地址返回 HTTP %d（%s）", resp.StatusCode, truncate(string(body), 40))
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("探测地址返回的不是 IP：%q", truncate(ip, 40))
	}
	return ip, nil
}
