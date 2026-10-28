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
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("[qwen2api] 配置加载失败: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	if len(cfg.Accounts) == 0 {
		log.Printf("[qwen2api] ⚠️ 未配置任何账号（config.accounts / QWEN2API_ACCOUNTS）—— " +
			"服务会启动，但所有生成请求将返回 503")
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
				cred, err := up.SignIn(ctx, a.Email, a.Password)
				if err != nil {
					log.Printf("[qwen2api] 账号 %s 登录失败: %v", a.Name, err)
					p.MarkAuth(p.Get(a.Name), err.Error())
					return
				}
				p.SetToken(a.Name, cred)
				log.Printf("[qwen2api] 账号 %s 登录成功（cookie 形态）", a.Name)
				return
			}
			mode, err := up.DetectAuthMode(ctx, a.Token)
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
func defaultConfigPath() string {
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
