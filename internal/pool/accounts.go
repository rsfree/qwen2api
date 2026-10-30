package pool

// 账号的热增删。
//
// 为什么要"热"：面板导入账号是运维动作，重启容器会切断所有在途会话
// （正在生成的长请求全废），而导入本身跟运行态无关。
// 所以增删只改内存 + 落 state/config，不重启进程。

import (
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// AddResult 一次导入的单条结果（直接给面板回显，字段名即 JSON）。
type AddResult struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Add 新增账号。同名已存在时返回 ok=false（不覆盖）。
//
// 为什么"重复"不算错误：导入脚本会反复跑同一批号，
// 把"已存在"报成失败会让人以为导入坏了。
func (p *Pool) Add(a config.Account) (*Account, bool) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return nil, false
	}
	p.mu.Lock()
	if p.lookupLocked(name) != nil {
		p.mu.Unlock()
		return nil, false
	}
	acc := &Account{
		Name:     name,
		Token:    a.Token,
		Email:    strings.TrimSpace(a.Email),
		Password: a.Password,
		Note:     a.Note,
	}
	p.accounts = append(p.accounts, acc)
	p.mu.Unlock()
	p.save()
	return acc, true
}

// Remove 移除账号。返回是否真的删掉了。
//
// 注意：删号不清 state 文件里该号的记录 —— 下次同名再加回来，
// 冷却与每日配额会"复活"。这是有意的：同名即同号，配额不该因为
// 删了又加就被重置（否则删一次就等于把上游额度刷新一遍）。
func (p *Pool) Remove(name string) bool {
	p.mu.Lock()
	idx := -1
	for i, a := range p.accounts {
		if a.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		p.mu.Unlock()
		return false
	}
	p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)
	p.mu.Unlock()
	p.save()
	return true
}

// Export 导出当前账号清单（含密码），用于回写配置文件。
//
// 顺序与内存一致（导入顺序），这样 diff 配置时能看出"新加的在末尾"。
func (p *Pool) Export() []config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]config.Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, config.Account{
			Name:     a.Name,
			Token:    a.Token,
			Email:    a.Email,
			Password: a.Password,
			Note:     a.Note,
		})
	}
	return out
}

// Has 账号是否存在（导入前判重用）。
func (p *Pool) Has(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lookupLocked(name) != nil
}
