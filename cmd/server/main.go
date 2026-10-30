// main.go qwen2api 入口：加载配置 → 建池 → 起 HTTP 服务 → 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/panel"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/server"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// version 由发版流水线用 -ldflags 注入；本地构建为 dev。
var version = "dev"

func main() {
	var (
		cfgPath = flag.String("config", defaultConfigPath(), "配置文件路径（JSON；也可只用环境变量）")
		listen  = flag.String("listen", "", "监听地址（覆盖配置文件，如 :8400）")
		showVer = flag.Bool("version", false, "打印版本后退出")
		passwd  = flag.String("passwd", "", "生成面板登录密码的 hash 后退出（用法：-passwd '你的密码'）")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	// 小的运维工具：把明文密码换成可直接粘进 config 的 hash。
	// 之所以内置而不是让用户自己算：算错迭代次数的格式会导致"密码永远不对"，
	// 而那个错误的表象是"登录失败"，极难查到根因。
	if *passwd != "" {
		h, err := config.HashPassword(*passwd)
		if err != nil {
			log.Fatalf("[qwen2api] 生成失败: %v", err)
		}
		fmt.Printf("把下面这段并入 config.json 的 panel（session_secret 建议另配一个随机串）：\n\n")
		fmt.Printf("  \"panel\": {\n    \"username\": \"admin\",\n    \"password_hash\": %q,\n    \"session_hours\": 12\n  }\n\n", h)
		fmt.Println("也可以只用环境变量：QWEN2API_PANEL_PASSWORD=<明文>（容器部署推荐）")
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("[qwen2api] 配置加载失败: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	// 面板托管的账号清单（accounts.json）：存在即**完全接管** config.json 的 accounts。
	// 规则见 internal/config/accounts.go —— 不合并是刻意的，否则"删掉的号"重启会复活。
	accPath := config.AccountsFile(cfg.StateFile)
	if accs, err := config.LoadAccountsOverride(accPath); err != nil {
		log.Printf("[qwen2api] ⚠️ %v —— 本次仍用 config.json 的账号，但面板里的删除可能在重启后复活", err)
	} else if accs != nil {
		cfg.Accounts = accs
		log.Printf("[qwen2api] 账号清单由面板托管：%s（%d 个）", accPath, len(accs))
	}

	// 配置来源必须打出来。没有这一行时，"--config 没传对 / 环境变量没生效"
	// 的表现是：服务正常启动、但账号列表和 api_keys 都是空的（静默用默认值），
	// 排查时只能靠猜。2026-10-29 容器部署实测踩过。
	if *cfgPath == "" {
		log.Printf("[qwen2api] 配置来源：无配置文件，仅使用环境变量 QWEN2API_*")
	} else {
		log.Printf("[qwen2api] 配置来源：%s", *cfgPath)
	}
	if len(cfg.Accounts) == 0 {
		log.Printf("[qwen2api] ⚠️ 未配置任何账号（config.accounts / QWEN2API_ACCOUNTS）—— " +
			"服务会启动，但所有生成请求将返回 503（用 -config 或 QWEN2API_CONFIG 指路）")
	}
	if len(cfg.APIKeys) == 0 {
		log.Printf("[qwen2api] ⚠️ 未配置 api_keys —— 前门不鉴权，请只在可信网络内暴露")
	}

	pool := pool.New(cfg.Pool, cfg.Accounts, cfg.StateFile)
	up := upstream.New(cfg.Upstream)
	// 启动时把账号"点亮"：只有密码的走 signin，有 token 的探测凭据形态。
	// 这两步都是一次只读请求，换来的是"第一个用户请求不用替我们试错"。
	bootstrapAccounts(cfg, pool, up)
	reqLog := reqlog.New(500)

	srv := server.New(cfg, pool, up, reqLog, version)
	srv.SetPanel(panel.New(srv))

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
		// 流式响应可能持续数分钟：只限读头/读体，不限写。
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.Default(),
	}

	log.Printf("[qwen2api] %s 启动：listen=%s accounts=%d(ready=%d) models=%s thinking=%s",
		version, cfg.Listen, pool.Len(), pool.ReadyCount(), cfg.DefaultModel, cfg.DefaultThinking)

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("[qwen2api] 收到退出信号，开始优雅关闭（30s）")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("[qwen2api] 关闭异常: %v", err)
		}
		close(idle)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("[qwen2api] 监听失败: %v", err)
	}
	<-idle
	log.Printf("[qwen2api] 已退出")
}

// bootstrapAccounts 并发登录/探测全部账号（有总超时，失败不影响启动）。
func bootstrapAccounts(cfg config.Config, p *pool.Pool, up *upstream.Client) {
	if len(cfg.Accounts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, a := range cfg.Accounts {
		wg.Add(1)
		go func(a config.Account) {
			defer wg.Done()
			if a.Token == "" {
				cred, err := up.SignIn(upstream.WithAccount(ctx, a.Name), a.Email, a.Password)
				if err != nil {
					log.Printf("[qwen2api] 账号 %s 登录失败: %v", a.Name, err)
					p.MarkAuth(p.Get(a.Name), err.Error())
					return
				}
				p.SetToken(a.Name, cred)
				log.Printf("[qwen2api] 账号 %s 登录成功（cookie 形态）", a.Name)
				return
			}
			mode, err := up.DetectAuthMode(upstream.WithAccount(ctx, a.Name), a.Token)
			if err != nil {
				log.Printf("[qwen2api] 账号 %s 凭据探测失败: %v", a.Name, err)
				p.MarkAuth(p.Get(a.Name), err.Error())
				return
			}
			p.SetAuthMode(a.Name, mode)
			log.Printf("[qwen2api] 账号 %s 凭据形态=%s", a.Name, mode)
		}(a)
	}
	wg.Wait()
}

// defaultConfigPath 优先 ./config.json，其次可执行文件同目录。
// defaultConfigPath 配置文件的默认位置，按"显式 > 环境变量 > 约定"三级回落。
//
// 🔴 QWEN2API_CONFIG 这一级是容器部署的必需品：镜像里既没有 cwd 的 config.json、
// 也不该把配置塞进 /usr/local/bin 旁边，只能靠环境变量指路。
// （2026-10-29 实测：只给 QWEN2API_CONFIG 时服务以"0 账号 + 不鉴权"启动 ——
// 它没报错，只是静默用默认值跑，比启动失败更难发现。）
func defaultConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("QWEN2API_CONFIG")); v != "" {
		return v
	}
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
