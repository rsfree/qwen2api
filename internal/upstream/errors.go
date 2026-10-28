package upstream

import (
	"errors"
	"fmt"
)

// Kind 上游错误的可机读分类 —— 账号池的处置策略按 Kind 分派，不靠文案匹配。
type Kind string

const (
	KindAuth      Kind = "auth"       // 凭据失效（401 / Unauthorized）⇒ 换号 + 长冷却
	KindQuota     Kind = "quota"      // 额度/积分耗尽 ⇒ 换号（未受理，重试安全）
	KindRisk      Kind = "risk"       // 风控（RGV587 / WAF 挑战页）⇒ 换号 + 软冷却
	KindRateLimit Kind = "rate_limit" // 429 ⇒ 软冷却
	KindBadInput  Kind = "bad_input"  // 请求体不合法（version 头缺失等）⇒ 不重试，直接 400
	KindServer    Kind = "server"     // 5xx ⇒ 不重试（可能已受理）
	KindTransport Kind = "transport"  // 网络/超时 ⇒ 不重试写端点（可能已受理）
	KindUnknown   Kind = "unknown"
)

// Error 上游错误。RetrySafe 表示「上游肯定没受理」⇒ 换号重试不会重复计费。
type Error struct {
	Kind       Kind
	Op         string
	Status     int
	Code       string
	Detail     string
	RetrySafe  bool
	RetryAfter int // 秒，0 = 用池的默认冷却
}

func (e *Error) Error() string {
	var sb string
	if e.Status > 0 {
		sb = fmt.Sprintf(" HTTP %d", e.Status)
	}
	code := ""
	if e.Code != "" {
		code = " " + e.Code
	}
	detail := ""
	if e.Detail != "" {
		detail = ": " + truncate(e.Detail, 240)
	}
	return fmt.Sprintf("%s:%s%s%s", e.Op, sb, code, detail)
}

// KindOf 取错误分类；非本包错误返回 KindUnknown。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

// RetrySafe 该错误是否可安全换号重试。
func RetrySafe(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.RetrySafe
	}
	return false
}

// StatusOf 上游 HTTP 状态码（0 = 未拿到响应）。
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// NewErr 构造上游错误。
func NewErr(kind Kind, op string, status int, code, detail string, retrySafe bool) *Error {
	return &Error{Kind: kind, Op: op, Status: status, Code: code, Detail: detail, RetrySafe: retrySafe}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ErrQuota 上游额度/权益耗尽（**流内信号**，不是 HTTP 语义错误）。
//
// 与 KindQuota 的区别：KindQuota 来自 HTTP 4xx（"这个号现在不能用"），
// ErrQuota 来自 SSE —— 上游把额度耗尽当成一句**正常的 answer 文本**下发
// （HTTP 200 + 正常收流，只是附 `extra.update_member: true`）。
//
// 只有"要图却没图"时才推得出来，所以判定放在图片链路，不塞进 classifyHTTP。
// 2026-10-28 实测：不识别它就会把"额度用完"报成"模型没画"，把调用方引向改 prompt。
var ErrQuota = errors.New("上游额度已用完")
