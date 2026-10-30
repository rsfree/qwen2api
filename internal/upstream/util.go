package upstream

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// UUID 生成 RFC 4122 v4 字符串（不引第三方依赖）。
func UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实践中等同系统不可用；退回时间戳保证不 panic。
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}

// TimezoneHeader 上游 timezone 头格式，实测形态：
//
//	"Wed Oct 28 2026 17:48:27 GMT+0800"
//
// 注意：Go 的 "MST" 布局给的是时区名（CST），不是 "GMT+0800"；必须手工拼。
func TimezoneHeader(t time.Time) string {
	t = t.In(time.FixedZone("CST", 8*3600))
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s %s %02d %d %02d:%02d:%02d GMT%s%02d%02d",
		t.Weekday().String()[:3],
		t.Month().String()[:3],
		t.Day(), t.Year(),
		t.Hour(), t.Minute(), t.Second(),
		sign, offset/3600, (offset%3600)/60)
}

// 凭据形态。🔴 2026-10-28 实测：两种形态**不能同时发** ——
// 带 Authorization: Bearer <无效值> 时，服务端不再回退去读 Cookie，直接 401。
// 所以形态必须显式确定，不能"两个都带上试试"。
const (
	ModeCookie = "cookie" // Cookie: token=<JWT>（signin 直接签发的形态）
	ModeBearer = "bearer" // Authorization: Bearer <token>（网页端当前形态）
)

// SplitCredential 解析凭据串，返回 (形态, 本体)。
//
// 支持三种写法：
//   - `cookie:<jwt>` / `bearer:<jwt>` —— 显式指定（探测后由池写回）；
//   - 含 ";" 或以 "token=" 开头 —— 完整 cookie jar，按 cookie 处理；
//   - 其余 —— 裸 token，形态未知（mode 返回空串，由调用方探测）。
func SplitCredential(cred string) (mode, value string) {
	c := strings.TrimSpace(cred)
	switch {
	case strings.HasPrefix(c, ModeCookie+":"):
		return ModeCookie, strings.TrimSpace(strings.TrimPrefix(c, ModeCookie+":"))
	case strings.HasPrefix(c, ModeBearer+":"):
		return ModeBearer, strings.TrimSpace(strings.TrimPrefix(c, ModeBearer+":"))
	case IsCookieJar(c):
		return ModeCookie, c
	default:
		return "", normalizeBearer(c)
	}
}

// TokenExpiry 从 JWT 的 exp 解出过期时间（**不验签**）。
//
// 只用于本地调度（选号优先"还能用"的、后台提前续期），不参与任何鉴权判断。
// 解不出 exp（非 JWT / 格式变了）时返回 ok=false，调用方按"未知"处理。
func TokenExpiry(cred string) (time.Time, bool) {
	_, v := SplitCredential(cred)
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &c); err != nil || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0), true
}

// IsCookieJar 判断凭据是否为完整 cookie jar：
// 含 ";" 或以 "token=" 开头。
func IsCookieJar(cred string) bool {
	c := strings.TrimSpace(cred)
	return strings.Contains(c, ";") || strings.HasPrefix(c, "token=")
}

// WithMode 把形态写回凭据串（池探测成功后调用）。
func WithMode(mode, value string) string {
	if mode == "" {
		return value
	}
	return mode + ":" + value
}

// normalizeBearer 去掉可能已经带上的 "Bearer " 前缀。
func normalizeBearer(cred string) string {
	c := strings.TrimSpace(cred)
	if len(c) > 7 && strings.EqualFold(c[:7], "bearer ") {
		return strings.TrimSpace(c[7:])
	}
	return c
}
