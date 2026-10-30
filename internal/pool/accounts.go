package pool

// 账号的热增删。
//
// 为什么要"热"：面板导入账号是运维动作，重启容器会切断所有在途会话
// （正在生成的长请求全废），而导入本身跟运行态无关。
// 所以增删只改内存 + 落 state/config，不重启进程。

import (
	"sort"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// expireOf 从凭据的 JWT exp 解出过期时刻。解不出（非 JWT）返回 ok=false。
func expireOf(cred string) (time.Time, bool) {
	return upstream.TokenExpiry(cred)
}

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

// ---------- 凭据续期（refresh_token） ----------

// SetRefresh 记录 refresh_token（可重复使用，用于换新的 access token）。
// 传空串表示清掉。
func (p *Pool) SetRefresh(name, refreshToken string) bool {
	p.mu.Lock()
	a := p.lookupLocked(name)
	if a == nil {
		p.mu.Unlock()
		return false
	}
	a.RefreshToken = strings.TrimSpace(refreshToken)
	p.mu.Unlock()
	p.save()
	return true
}

// RefreshToken 取账号的 refresh_token（没有则空串）。
func (p *Pool) RefreshToken(name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if a := p.lookupLocked(name); a != nil {
		return a.RefreshToken
	}
	return ""
}

// TokenExpiry 取当前 access token 的过期时刻（零值/false = 未知）。
func (p *Pool) TokenExpiry(name string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	a := p.lookupLocked(name)
	if a == nil || a.tokenExp.IsZero() {
		return time.Time{}, false
	}
	return a.tokenExp, true
}

// WarmCandidates 返回"接下来会被用到、且凭据快过期"的账号名。
//
// 为什么要按 served 排序：选号也是按 served 升序取的，所以这里取前 target 个
// 正好就是**下一步要用的那批**。提前把它们的 token 换新，请求路径上就不用
// 停下来等 refresh（这就是"预热预取、不影响性能"）。
//
// target 由调用方按当前流量估算；window 是"多快过期算快过期"；
// limit 是单轮上限（防止一次刷太多把上游打热）。
func (p *Pool) WarmCandidates(target int, window time.Duration, limit int) []string {
	if target <= 0 || limit <= 0 {
		return nil
	}
	type row struct {
		name   string
		served int64
		exp    time.Time
	}
	p.mu.RLock()
	rows := make([]row, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.authDead || a.RefreshToken == "" {
			continue
		}
		rows = append(rows, row{name: a.Name, served: a.served, exp: a.tokenExp})
	}
	p.mu.RUnlock()

	// 与选号同序：用得最少的排前面。
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].served != rows[j].served {
			return rows[i].served < rows[j].served
		}
		return rows[i].name < rows[j].name
	})
	if target > len(rows) {
		target = len(rows)
	}
	now := time.Now()
	out := make([]string, 0, limit)
	for _, r := range rows[:target] {
		// exp 为零 = 未知（配置里手写的长期凭据），不去动它。
		if r.exp.IsZero() || now.Before(r.exp.Add(-window)) {
			continue
		}
		out = append(out, r.name)
		if len(out) >= limit {
			break
		}
	}
	return out
}
