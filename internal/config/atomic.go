package config

// 面板托管文件的原子写。
//
// 为什么所有托管文件（accounts.json / apikeys.json）都走这里：
//   - 直接 os.WriteFile 会在写一半时断电/被杀，留下半截 JSON —— 下次启动直接解析失败，
//     表现是"面板里所有账号都消失"，比写失败严重得多；
//   - 同目录临时文件 + rename 是 POSIX 上的原子替换语义，读者要么看到旧的、要么看到新的。
//
// 同目录是必须的：跨设备 rename 会 EXDEV（/tmp 与 /app/data 通常是两个文件系统）。

import (
	"os"
	"path/filepath"
)

// writeFileAtomic 把 body 原子写到 path，权限 0600（里面是明文凭据）。
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里什么也不做

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
