package config

// 面板托管的账号清单。
//
// 为什么不直接回写 config.json：
//   - 部署时 config.json 是**只读挂载**（`:ro`），写回直接 EROFS；
//   - 就算可写，让程序去改用户的声明式配置也太粗暴 —— 一个格式 bug
//     就能把 default_model、pool 参数一起写坏。
//
// 所以分成两层：
//   config.json    声明式基线（只读，可入库前的模板）
//   accounts.json  面板托管（导入/删除写这里，**存在即完全接管**账号列表）
//
// "完全接管"而不是"合并"是刻意的：合并会让"删掉的号又回来了"这种
// 语义模糊（删了要不要从 config 再合进来？）。接管规则一句话说得清。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AccountsFileName 面板托管文件名。
const AccountsFileName = "accounts.json"

// AccountsFile 返回托管文件路径：与 state_file 同目录（部署里是 /app/data）。
//
// state_file 为空时退回当前目录 —— 单机跑二进制也能用。
func AccountsFile(stateFile string) string {
	dir := strings.TrimSpace(filepath.Dir(stateFile))
	if dir == "" || dir == "." {
		return AccountsFileName
	}
	return filepath.Join(dir, AccountsFileName)
}

// LoadAccountsOverride 读面板托管的账号清单。
//
// 返回值语义：
//   - (nil, nil)：文件不存在 ⇒ 用 config.json 里的 accounts（从未用面板导入过）
//   - (accounts, nil)：文件存在 ⇒ **完全接管**（可能是空数组：用户把号全删了）
//   - (nil, err)：文件存在但坏了 ⇒ 调用方应报错而不是静默退回 config
//
// 第三种刻意不吞：静默退回 config 会让"面板里删掉的号"在重启后复活。
func LoadAccountsOverride(path string) ([]Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读 %s: %w", path, err)
	}
	var out []Account
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 %s: %w（修好或删掉它；删掉即退回 config.json 的账号）", path, err)
	}
	if out == nil {
		out = []Account{}
	}
	return out, nil
}

// SaveAccounts 原子写托管清单（0600，同目录临时文件 + rename）。
//
// 同目录是必须的：跨设备 rename 会 EXDEV，写到 /tmp 再搬会失败。
func SaveAccounts(path string, accounts []Account) error {
	if accounts == nil {
		accounts = []Account{}
	}
	body, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里什么也不做

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
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
