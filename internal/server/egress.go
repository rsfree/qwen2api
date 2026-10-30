package server

// 出口 IP 的缓存与后台探测。
//
// 请求流水里有两个"IP"，含义完全不同，之前只记了前者：
//   - client_ip：**谁**调了我们（调用方/浏览器）；
//   - egress_ip：我们**用哪个 IP** 去访问上游（账号对应的 socks5 出口）。
//
// 排障时后者才是关键：同一个上游错误，在"干净 IP"上只是偶发，在"被风控的 IP"上
// 会稳定复现。而出口 IP 只能实测（socks5h 的出口由运营商分配），所以这里做
// "懒探测 + 缓存"，绝不阻塞主链路。

import (
	"context"
	"log"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
)

const (
	// egressTTL 出口 IP 的缓存时长。运营商侧出口会漂，但不会分钟级漂；
	// 30 分钟足够定位"今天这批请求是从哪出去的"，也避免探测太频繁。
	egressTTL = 30 * time.Minute
	// egressProbeTimeout 单次探测的硬上限（探测本身还有 8s，这里再兜一层）。
	egressProbeTimeout = 10 * time.Second
)

// egressState 只记"什么时候探过、正在探没探"；
// IP 本身存在 pool 里（唯一真源），避免两处各存一份还要同步。
type egressState struct {
	at      time.Time
	probing bool
}

// egressOf 返回账号最近一次实测的出口 IP；过期或未知时**异步**补一次。
//
// 返回空串是正常的：首次使用某账号时还没探过，调用方（流水）先留空，
// 下一轮请求就能看到（面板本来就在轮询刷新）。
func (s *Server) egressOf(account string) string {
	if account == "" {
		return ""
	}
	s.egressMu.Lock()
	st := s.egress[account]
	if st == nil {
		st = &egressState{}
		s.egress[account] = st
	}
	needProbe := !st.probing && time.Since(st.at) > egressTTL
	if needProbe {
		st.probing = true
	}
	s.egressMu.Unlock()

	if needProbe {
		go s.probeEgress(account)
	}
	// 缓存值在 pool 里（过了 egressTTL 会被当作未知，和探测节奏一致）。
	return s.pool.EgressIP(account, egressTTL)
}

// probeEgress 后台探一次，成功才覆盖（失败保留旧值，并记时间避免风暴）。
func (s *Server) probeEgress(account string) {
	ctx, cancel := context.WithTimeout(context.Background(), egressProbeTimeout)
	defer cancel()

	ip, err := s.up.EgressIP(ctx, account)

	s.egressMu.Lock()
	st := s.egress[account]
	if st == nil {
		st = &egressState{}
		s.egress[account] = st
	}
	st.probing = false
	st.at = time.Now() // 失败也记：否则每个请求都会再触一次探测
	s.egressMu.Unlock()

	if err != nil {
		// 探测失败不该刷屏：只记一行，且不影响任何请求。
		log.Printf("[qwen2api] 出口 IP 探测失败（%s）: %v", account, err)
		return
	}
	s.pool.SetEgressIP(account, ip)
}

// addReq 落一条请求流水（顺手补出口 IP）。
//
// 统一收口的原因：流水里"谁调的/哪个号/哪个出口"必须一起出现，
// 否则排障时看到 acct= 空或没有出口，等于没有线索。
func (s *Server) addReq(e *reqlog.Entry) {
	if e.Account != "" {
		e.EgressIP = s.egressOf(e.Account)
	}
	s.log.Add(*e)
}

// EgressOf 供面板/测试查询某账号的出口 IP（空 = 未知）。
func (s *Server) EgressOf(account string) string {
	return s.pool.EgressIP(account, egressTTL)
}
