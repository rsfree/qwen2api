package server

// 面板侧的账号写操作：导入 / 删除 / 单个重登。
//
// 🔴 落盘位置是 `accounts.json`（与 state_file 同目录），**不是 config.json** ——
// 部署时 config.json 是只读挂载，而且它承载"声明式配置"，不该被运行时改写。
// accounts.json 一旦存在就**完全接管**账号列表（见 config/accounts.go 的说明）。

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ImportAccounts 批量导入：先逐条校验（能登录成功才算数），成功的入池后统一落盘。
//
// validate=false 时跳过登录校验（只有明确知道凭据有效时才关，比如已有 token）。
// 默认必须校验：导入一条错密码的号，等于给池子塞一颗哑弹 —— 它会被选号选中、
// 请求失败、然后冷却，表面看像上游抖动。
func (s *Server) ImportAccounts(ctx context.Context, in []config.Account, validate bool) ([]pool.AddResult, error) {
	results := make([]pool.AddResult, len(in))
	// 预分配名字（顺序做，避免并发下重名）
	used := map[string]bool{}
	for name := range s.poolNames() {
		used[name] = true
	}
	type job struct {
		idx  int
		acc  config.Account
		skip string
	}
	var jobs []job
	for i, a := range in {
		email := strings.TrimSpace(a.Email)
		pw := strings.TrimSpace(a.Password)
		res := pool.AddResult{Email: email}
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = defaultAccountName(email, i)
		}
		res.Name = name
		switch {
		case email == "" || !strings.Contains(email, "@"):
			res.Error = "邮箱缺失或格式不对"
			results[i] = res
			continue
		case pw == "":
			res.Error = "密码缺失（本仓用邮箱+密码登录，纯 token 形态请直接写进 config.json）"
			results[i] = res
			continue
		case s.pool.Has(name), used[name]:
			res.Error = "账号名已存在（已跳过，未改动原账号）"
			results[i] = res
			continue
		case s.poolHasEmail(email):
			res.Error = "该邮箱已在池中（跳过）"
			results[i] = res
			continue
		}
		used[name] = true
		jobs = append(jobs, job{idx: i, acc: config.Account{
			Name: name, Email: email, Password: pw, Note: strings.TrimSpace(a.Note),
		}})
	}

	// 4 路并发登录：串行 10 个号 ≈ 15s，50 个号就 1 分钟以上，面板会像卡死。
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var okCreds = map[int]string{}
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := pool.AddResult{Name: j.acc.Name, Email: j.acc.Email}
			if validate {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				// 登录也要走该账号的固定出口 —— 登录 IP 与使用 IP 不一致本身
				// 就是风控画像里的可疑信号。
				cred, err := s.up.SignIn(upstream.WithAccount(cctx, j.acc.Name), j.acc.Email, j.acc.Password)
				if err != nil {
					res.Error = friendlySigninErr(err)
					mu.Lock()
					results[j.idx] = res
					mu.Unlock()
					return
				}
				mu.Lock()
				okCreds[j.idx] = cred
				mu.Unlock()
			}
			res.OK = true
			mu.Lock()
			results[j.idx] = res
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	// 入池（顺序，持锁时间短）
	imported := 0
	for _, j := range jobs {
		if !results[j.idx].OK {
			continue
		}
		if _, added := s.pool.Add(j.acc); !added {
			results[j.idx].OK = false
			results[j.idx].Error = "入池时发现重名"
			continue
		}
		if cred := okCreds[j.idx]; cred != "" {
			s.pool.SetToken(j.acc.Name, cred)
			s.pool.Reset(j.acc.Name)
		}
		imported++
	}

	if imported == 0 {
		return results, nil
	}
	// 统一落盘一次（不是每条一次：10 条就 10 次原子写，纯浪费）。
	if err := s.saveAccounts(); err != nil {
		log.Printf("[qwen2api] 账号已入池但落盘失败（重启后会丢）: %v", err)
		return results, fmt.Errorf("账号已入池，但写 %s 失败：%w", s.accountsPath(), err)
	}
	log.Printf("[qwen2api] 面板导入 %d 个账号（校验=%v），当前池 %d 个", imported, validate, s.pool.Len())
	return results, nil
}

// DeleteAccount 从池中移除并落盘。
func (s *Server) DeleteAccount(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("缺少账号名")
	}
	if !s.pool.Remove(name) {
		return false, nil
	}
	if err := s.saveAccounts(); err != nil {
		return true, fmt.Errorf("已从池中移除，但写 %s 失败：%w", s.accountsPath(), err)
	}
	log.Printf("[qwen2api] 面板删除账号 %s，当前池 %d 个", name, s.pool.Len())
	return true, nil
}

// TestAccount 用密码重新登录一次，成功则刷新 token 并清掉冷却/失效标记。
func (s *Server) TestAccount(ctx context.Context, name string) error {
	a := s.pool.Get(name)
	if a == nil {
		return fmt.Errorf("账号不存在")
	}
	if strings.TrimSpace(a.Password) == "" {
		return fmt.Errorf("该账号没有保存密码，无法重登（token 形态请在 config.json 里更新）")
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cred, err := s.up.SignIn(upstream.WithAccount(cctx, name), a.Email, a.Password)
	if err != nil {
		s.pool.MarkAuth(a, err.Error())
		return fmt.Errorf("登录失败: %w", err)
	}
	s.pool.SetToken(name, cred)
	s.pool.Reset(name)
	return nil
}

// Models 给面板的模型清单（复用前门同一份缓存，不额外打上游）。
func (s *Server) Models(ctx context.Context) any { return s.models.List(ctx) }

// ---------- 内部 ----------

// friendlySigninErr 把上游的原始报错翻成能指导处置的话。
//
// 上游 400 会带回一整段 JSON（request_id/error 嵌套），直接塞给用户等于没说。
// 但要保留"可判断"的信息 —— 所以是**翻译**不是吞掉：无法识别的仍原样给（截断）。
func friendlySigninErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "Bad_Request"), strings.Contains(s, "Unauthorized"):
		return "邮箱或密码错误（上游拒登）"
	case strings.Contains(s, "aliyun_waf"), strings.Contains(s, "RGV587"):
		return "被上游风控拦截（同出口连登过多，稍后再试）"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "登录超时（网络或上游抖动）"
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return "登录失败: " + s
}

// accountsPath 面板托管账号文件的位置。
func (s *Server) accountsPath() string { return config.AccountsFile(s.cfg.StateFile) }

// saveAccounts 把池内账号写成 accounts.json。
func (s *Server) saveAccounts() error {
	s.accountsMu.Lock()
	defer s.accountsMu.Unlock()
	return config.SaveAccounts(s.accountsPath(), s.pool.Export())
}

func (s *Server) poolNames() map[string]struct{} {
	out := map[string]struct{}{}
	for _, a := range s.pool.Export() {
		out[a.Name] = struct{}{}
	}
	return out
}

func (s *Server) poolHasEmail(email string) bool {
	for _, a := range s.pool.Export() {
		if strings.EqualFold(a.Email, email) {
			return true
		}
	}
	return false
}

// defaultAccountName 没给名字时从邮箱推一个：`zhang.san+x@qq.com` → `zhang.sanx`。
//
// 只保留 [a-z0-9._-]，其余（含中文/大写）丢掉；空则用序号。
func defaultAccountName(email string, idx int) string {
	local := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		local = email[:at]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "._-")
	if name == "" {
		return fmt.Sprintf("acc%d", idx+1)
	}
	return name
}
