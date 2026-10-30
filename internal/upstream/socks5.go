package upstream

// 最小 SOCKS5 客户端（RFC 1928 建连 + RFC 1929 用户名密码认证）。
//
// 为什么自己写而不是用 http.Transport.Proxy 的 socks5：
//   1. 本仓至今零第三方依赖，为一条隧道拉 x/net 不划算；
//   2. stdlib 对 socks5 的支持随版本变（认证、ATYP 处理），而这里需要**确定性**：
//      域名必须原样交给代理去解析（socks5h 语义），否则在跨境链路上会解析成
//      错误区域的 IP，表现为"能连但被风控"这种很难查的现象。
//
// 只实现 CONNECT + 域名/IP 两种 ATYP，够用且好测。

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	socks5Version = 0x05

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04
)

// socks5Dial 经 proxyAddr（host:port）连到 target（host:port）。
//
// target 是域名时会以 ATYP=domain 发出去 —— 由代理解析（等价 socks5h）。
func socks5Dial(ctx context.Context, proxyAddr, user, pass, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("socks5: 目标地址不合法 %q: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("socks5: 端口不合法 %q", portStr)
	}

	d := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5: 连代理失败: %w", err)
	}
	// 握手期间必须有时限：否则代理不回包会把请求挂死在这里（比报错更难查）。
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}
	// 握手成功后清掉自设的期限，交给上层（流式响应可能几分钟）。
	cleanup := func(e error) (net.Conn, error) {
		conn.Close()
		return nil, e
	}

	// 1) 问候：按是否有凭据决定提供哪些方法
	methods := []byte{methodNoAuth}
	if user != "" {
		methods = []byte{methodUserPass, methodNoAuth}
	}
	greet := append([]byte{socks5Version, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return cleanup(fmt.Errorf("socks5: 发送问候失败: %w", err))
	}
	var resp [2]byte
	if _, err := io.ReadFull(conn, resp[:]); err != nil {
		return cleanup(fmt.Errorf("socks5: 读问候响应失败: %w", err))
	}
	if resp[0] != socks5Version {
		return cleanup(fmt.Errorf("socks5: 代理版本不是 5（%d）", resp[0]))
	}
	switch resp[1] {
	case methodNoAuth:
		// 免认证
	case methodUserPass:
		if err := socks5Auth(conn, user, pass); err != nil {
			return cleanup(err)
		}
	case methodNoAcceptable:
		return cleanup(errors.New("socks5: 代理拒绝所有认证方式（用户名/密码缺失或不匹配）"))
	default:
		return cleanup(fmt.Errorf("socks5: 代理要求不支持的认证方式 0x%02x", resp[1]))
	}

	// 2) CONNECT
	req := []byte{socks5Version, cmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, atypIPv4)
			req = append(req, v4...)
		} else {
			req = append(req, atypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return cleanup(fmt.Errorf("socks5: 域名过长: %s", host))
		}
		req = append(req, atypDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return cleanup(fmt.Errorf("socks5: 发送 CONNECT 失败: %w", err))
	}

	// 3) 应答：VER REP RSV ATYP [BND.ADDR] BND.PORT
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return cleanup(fmt.Errorf("socks5: 读 CONNECT 应答失败: %w", err))
	}
	if head[0] != socks5Version {
		return cleanup(fmt.Errorf("socks5: CONNECT 应答版本不对（%d）", head[0]))
	}
	if head[1] != 0x00 {
		return cleanup(fmt.Errorf("socks5: CONNECT 被拒（%s）", socks5ErrText(head[1])))
	}
	// 把 BND 部分读完，否则后续读到的第一段数据会错位
	switch head[3] {
	case atypIPv4:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return cleanup(fmt.Errorf("socks5: 读 BND 失败: %w", err))
		}
	case atypIPv6:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return cleanup(fmt.Errorf("socks5: 读 BND 失败: %w", err))
		}
	case atypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return cleanup(fmt.Errorf("socks5: 读 BND 失败: %w", err))
		}
		if _, err := io.ReadFull(conn, make([]byte, int(l[0])+2)); err != nil {
			return cleanup(fmt.Errorf("socks5: 读 BND 失败: %w", err))
		}
	default:
		return cleanup(fmt.Errorf("socks5: 未知 ATYP 0x%02x", head[3]))
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socks5Auth(conn net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("socks5: 用户名或密码超长（最多 255 字节）")
	}
	buf := []byte{0x01, byte(len(user))}
	buf = append(buf, user...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, pass...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: 发送认证失败: %w", err)
	}
	var r [2]byte
	if _, err := io.ReadFull(conn, r[:]); err != nil {
		return fmt.Errorf("socks5: 读认证响应失败: %w", err)
	}
	if r[1] != 0x00 {
		return errors.New("socks5: 认证失败（用户名或密码错误）")
	}
	return nil
}

func socks5ErrText(code byte) string {
	switch code {
	case 0x01:
		return "代理内部错误"
	case 0x02:
		return "规则不允许"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "连接被拒"
	case 0x06:
		return "TTL 过期"
	case 0x07:
		return "不支持的命令"
	case 0x08:
		return "不支持的地址类型"
	default:
		return fmt.Sprintf("未知错误 0x%02x", code)
	}
}
