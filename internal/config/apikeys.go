package config

// 面板托管的 API Key 清单（前门令牌）。
//
// 与 accounts.json 同一套哲学：
//
//	config.json    声明式基线（api_keys，部署时通常 :ro 挂载）
//	apikeys.json   面板托管（生成/撤销写这里，**存在即完全接管**）
//
// 为什么需要托管：api_keys 就是"调用模型的 token"，而它此前只存在于 config.json /
// 环境变量里 —— 面板上看不到、改一把要改文件再重启、还容易把 sk- 开头的串记混。
// 托管之后：面板里能看（掩码 + 按需展开）、能生成、能撤销，**撤销立即生效**（不用重启）。
//
// 接管规则与账号完全一致：文件存在就以它为准，包括空数组（= 用户把 key 全删了，
// 此时前门不鉴权 —— 服务端会拒绝删到最后一把，只有手改文件才会出现这种状态）。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// APIKeysFileName 面板托管的 API Key 文件名。
const APIKeysFileName = "apikeys.json"

// APIKey 一把前门令牌。
type APIKey struct {
	Key     string    `json:"key"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
}

// APIKeysFile 返回托管文件路径：与 state_file 同目录（部署里是 /app/data）。
func APIKeysFile(stateFile string) string {
	dir := strings.TrimSpace(filepath.Dir(stateFile))
	if dir == "" || dir == "." {
		return APIKeysFileName
	}
	return filepath.Join(dir, APIKeysFileName)
}

// LoadAPIKeysOverride 读面板托管的 key 清单。
//
// 返回值语义与 LoadAccountsOverride 一致：
//   - (nil, nil)：文件不存在 ⇒ 用 config.json / 环境变量里的 api_keys；
//   - (keys, nil)：文件存在 ⇒ **完全接管**（可能是空数组）；
//   - (nil, err)：文件存在但坏了 ⇒ 调用方应报错，而不是静默退回 config
//     （静默退回会让"面板里撤销掉的 key"在重启后复活）。
//
// 两种写法都认，方便手写：
//
//	[{"key":"sk-qwen-…","note":"脚本"}, {"key":"sk-xxx"}]
//	["sk-qwen-…", "sk-xxx"]
func LoadAPIKeysOverride(path string) ([]APIKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读 %s: %w", path, err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("解析 %s: %w（修好或删掉它；删掉即退回 config.json 的 api_keys）", path, err)
	}

	out := make([]APIKey, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		var obj APIKey
		var plain string
		if err := json.Unmarshal(it, &obj); err == nil && strings.TrimSpace(obj.Key) != "" {
			// 对象形态
		} else if err := json.Unmarshal(it, &plain); err == nil {
			obj = APIKey{Key: strings.TrimSpace(plain)}
		} else {
			return nil, fmt.Errorf("解析 %s: 第 %d 项既不是字符串也不是 {key:…}", path, len(out)+1)
		}
		k := strings.TrimSpace(obj.Key)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if obj.Created.IsZero() {
			obj.Created = time.Now().UTC()
		}
		obj.Key = k
		obj.Note = strings.TrimSpace(obj.Note)
		out = append(out, obj)
	}
	return out, nil
}

// SaveAPIKeys 原子写托管清单（0600）。
func SaveAPIKeys(path string, keys []APIKey) error {
	if keys == nil {
		keys = []APIKey{}
	}
	body, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(body, '\n'))
}

// GenerateAPIKey 生成一把 `sk-qwen-<32 hex>`。
//
// 为什么是 128 位随机而不是 UUID：令牌会被贴进各种客户端配置里，
// 短、无连字符、前缀能一眼看出是哪套网关 —— 排障时省一次"这是谁的 key"。
func GenerateAPIKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-qwen-" + hex.EncodeToString(b), nil
}

// MaskAPIKey 掩码：留前 8 位与后 4 位，中间省略。
//
// 太短的串直接整体打点 —— 一个 10 位的 key 露 12 个字符就等于没掩。
func MaskAPIKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return strings.Repeat("•", len(k))
	}
	return k[:8] + "…" + k[len(k)-4:]
}
