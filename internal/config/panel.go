package config

// 面板密码的哈希与校验。
//
// 为什么自己写 PBKDF2 而不是引第三方：本仓至今**零第三方依赖**（go.mod 只有 module 行），
// 为一个小功能拉 x/crypto 不划算。PBKDF2-HMAC-SHA256 的核心也就十几行。
//
// 为什么不是"直接存明文就行"：配置文件虽然是 0600，但它会被复制、会被贴进 issue、
// 会跟着 docker volume 备份走。哈希能把这些场景的损失降到"只能暴力猜"。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// defaultPBKDF2Iter 迭代次数。12 万次 ≈ 单核 50ms，
// 对登录体验无感，对离线爆破是 12 万倍成本。
const defaultPBKDF2Iter = 120000

// HashPassword 生成 `pbkdf2$<iter>$<salt-hex>$<hash-hex>`。
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk := pbkdf2SHA256([]byte(password), salt, defaultPBKDF2Iter)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", defaultPBKDF2Iter,
		hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
}

// VerifyPanelPassword 校验面板密码。PasswordHash 优先于 Password。
//
// 比较一律用 subtle.ConstantTimeCompare：早退式比较会把"前缀对了几个字符"
// 泄给攻击者，几百次请求就能把哈希猜成前缀树。
func VerifyPanelPassword(p Panel, password string) bool {
	if h := strings.TrimSpace(p.PasswordHash); h != "" {
		iter, salt, want, ok := parsePasswordHash(h)
		if !ok {
			return false
		}
		got := pbkdf2SHA256([]byte(password), salt, iter)
		return subtle.ConstantTimeCompare(got, want) == 1
	}
	plain := strings.TrimSpace(p.Password)
	if plain == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(plain), []byte(password)) == 1
}

func parsePasswordHash(h string) (iter int, salt, want []byte, ok bool) {
	parts := strings.Split(h, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return 0, nil, nil, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1000 || n > 10_000_000 {
		return 0, nil, nil, false
	}
	salt, err = hex.DecodeString(parts[2])
	if err != nil || len(salt) < 8 {
		return 0, nil, nil, false
	}
	want, err = hex.DecodeString(parts[3])
	if err != nil || len(want) != sha256.Size {
		return 0, nil, nil, false
	}
	return n, salt, want, true
}

// pbkdf2SHA256 单块实现（输出 32 字节）。
func pbkdf2SHA256(password, salt []byte, iter int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	u := make([]byte, hLen)
	t := make([]byte, hLen)

	prf.Write(salt)
	// block index = 1（大端）
	prf.Write([]byte{0, 0, 0, 1})
	u = prf.Sum(u[:0])
	copy(t, u)

	for i := 1; i < iter; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(u[:0])
		for j := range t {
			t[j] ^= u[j]
		}
	}
	return t
}
