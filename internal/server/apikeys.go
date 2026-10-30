package server

// 前门令牌（API Key）的运行时管理。
//
// 来源有两层，规则与账号完全一致：
//
//	config.json / QWEN2API_API_KEYS   声明式基线
//	data/apikeys.json                 面板托管（存在即完全接管，撤销立即生效）
//
// 为什么不重启就要生效：key 是"调用模型的 token"，用户撤销一把泄漏的 key
// 时最不能接受的就是"等我有空重启一下"。这里全部走内存 + 原子落盘，
// 校验路径每次读最新快照（读锁，成本可忽略）。

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// SetAPIKeys 注入启动时解析出的生效 key（managed = apikeys.json 存在并接管）。
func (s *Server) SetAPIKeys(keys []config.APIKey, path string, managed bool) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if path == "" {
		path = config.APIKeysFile(s.cfg.StateFile)
	}
	s.keys = normalizeKeys(keys)
	s.keysPath = path
	s.keysManaged = managed
	s.keysInit = true
}

// APIKeys 当前生效的 key 快照（含明文，仅面板鉴权后可见）。
func (s *Server) APIKeys() []config.APIKey {
	s.keysMu.RLock()
	defer s.keysMu.RUnlock()
	if s.keysInit {
		out := make([]config.APIKey, len(s.keys))
		copy(out, s.keys)
		return out
	}
	// 未调用 SetAPIKeys（测试或极简嵌入）时退回配置里的值。
	out := make([]config.APIKey, 0, len(s.cfg.APIKeys))
	for _, k := range s.cfg.APIKeys {
		out = append(out, config.APIKey{Key: k})
	}
	return out
}

// APIKeysManaged apikeys.json 是否已接管（文件存在，或面板里生成/撤销过）。
func (s *Server) APIKeysManaged() bool {
	s.keysMu.RLock()
	defer s.keysMu.RUnlock()
	return s.keysManaged
}

// APIKeysPath 托管文件路径（面板展示用）。
func (s *Server) APIKeysPath() string {
	s.keysMu.RLock()
	defer s.keysMu.RUnlock()
	if s.keysPath != "" {
		return s.keysPath
	}
	return config.APIKeysFile(s.cfg.StateFile)
}

// AddAPIKey 生成一把新 key 并落盘。
//
// 首次调用会把当前生效的 key（来自 config / 环境变量）"领养"进 apikeys.json ——
// 否则生成一把新 key 的瞬间，老 key 会集体失效，客户端全断。领养之后
// 文件完全接管，后续删除才有意义（能删到只剩新的那把）。
func (s *Server) AddAPIKey(note string) (config.APIKey, error) {
	k, err := config.GenerateAPIKey()
	if err != nil {
		return config.APIKey{}, err
	}

	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	s.adoptLocked()

	item := config.APIKey{Key: k, Note: strings.TrimSpace(note), Created: time.Now().UTC()}
	next := make([]config.APIKey, 0, len(s.keys)+1)
	next = append(next, s.keys...)
	next = append(next, item)

	if err := config.SaveAPIKeys(s.keysPath, next); err != nil {
		return config.APIKey{}, err
	}
	s.keys = next
	// 日志只出现掩码：完整 key 一旦进日志就等于泄漏（日志会被收集、被贴进 issue）。
	log.Printf("[qwen2api] 已生成 API Key %s → %s（立即生效）", config.MaskAPIKey(k), s.keysPath)
	return item, nil
}

// DeleteAPIKey 撤销一把 key 并落盘。
func (s *Server) DeleteAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("key 为空")
	}

	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	s.adoptLocked()

	idx := -1
	for i, k := range s.keys {
		if k.Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("没有这把 key（可能已被撤销）")
	}
	// 🔴 绝不允许删到 0 把：那等于把前门改成"不鉴权"，任何人拿到地址就能烧账号额度。
	if len(s.keys) <= 1 {
		return errors.New("至少保留一把 key；确实想关闭鉴权，请删掉 apikeys.json 并把 config.api_keys 置空")
	}

	next := make([]config.APIKey, 0, len(s.keys)-1)
	next = append(next, s.keys[:idx]...)
	next = append(next, s.keys[idx+1:]...)
	if err := config.SaveAPIKeys(s.keysPath, next); err != nil {
		return err
	}
	s.keys = next
	log.Printf("[qwen2api] 已撤销 API Key %s（%s）", config.MaskAPIKey(key), s.keysPath)
	return nil
}

// adoptLocked 把当前生效的 key 复制进托管列表（未初始化时）。
func (s *Server) adoptLocked() {
	if !s.keysInit {
		s.keys = make([]config.APIKey, 0, len(s.cfg.APIKeys))
		for _, k := range s.cfg.APIKeys {
			s.keys = append(s.keys, config.APIKey{Key: k, Note: "来自配置文件"})
		}
		s.keysInit = true
	}
	if s.keysPath == "" {
		s.keysPath = config.APIKeysFile(s.cfg.StateFile)
	}
	s.keysManaged = true
}

// keyStrings 校验用的字符串视图。
func (s *Server) keyStrings() []string {
	ks := s.APIKeys()
	if len(ks) == 0 {
		return nil
	}
	out := make([]string, 0, len(ks))
	for _, k := range ks {
		out = append(out, k.Key)
	}
	return out
}

// normalizeKeys 去空白、去空项、去重（手改 apikeys.json 时很容易留重复行）。
func normalizeKeys(in []config.APIKey) []config.APIKey {
	seen := make(map[string]bool, len(in))
	out := make([]config.APIKey, 0, len(in))
	for _, k := range in {
		key := strings.TrimSpace(k.Key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		k.Key = key
		k.Note = strings.TrimSpace(k.Note)
		if k.Created.IsZero() {
			k.Created = time.Now().UTC()
		}
		out = append(out, k)
	}
	return out
}
