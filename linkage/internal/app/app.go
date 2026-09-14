// Package app 装配联动服务各组件，供 cmd 入口与集成测试复用。
package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"kernel-hw-linkage/internal/agent"
	"kernel-hw-linkage/internal/buffer"
	"kernel-hw-linkage/internal/config"
	"kernel-hw-linkage/internal/dedup"
	"kernel-hw-linkage/internal/es"
	"kernel-hw-linkage/internal/llm"
	"kernel-hw-linkage/internal/notify"
	"kernel-hw-linkage/internal/pipeline"
	"kernel-hw-linkage/internal/server"
)

// App 联动服务装配体。
type App struct {
	Cfg     *config.Config
	srv     *server.Server
	agg     *buffer.Aggregator
	httpSrv *http.Server
}

// New 按配置装配全部组件。
func New(cfg *config.Config) *App {
	esC := es.New(cfg.ES.Addr, cfg.ES.User, cfg.ES.Pass, cfg.ES.Index, 5*time.Second)
	// 分析规则：优先加载共享的 prompts/analyze.txt（与 agent skill 单一事实源）
	system := loadPrompt(cfg.LLM.PromptFile)
	llmC := llm.New(cfg.LLM.BaseURL, cfg.LLM.Model, cfg.LLM.APIKey, system, time.Duration(cfg.LLM.TimeoutS)*time.Second)
	note := notify.NewMulti(cfg)
	deduper := dedup.NewMemory(time.Duration(cfg.Window.DedupMinutes) * time.Minute)

	// 可选：告警直接交给外部 agent（加载 skill 检索+分析）
	var ag *agent.HTTPInvoker
	if cfg.Agent.Enabled && cfg.Agent.ExecutorURL != "" {
		timeout := cfg.Agent.TimeoutS
		if timeout <= 0 {
			timeout = 120
		}
		ag = agent.NewHTTP(cfg.Agent.ExecutorURL, cfg.Agent.Skill, time.Duration(timeout)*time.Second)
		log.Printf("I! agent executor enabled: %s skill=%s", cfg.Agent.ExecutorURL, cfg.Agent.Skill)
	}

	proc := pipeline.New(cfg, esC, llmC, note, ag, cfg.Window.MaxConcurrent)
	agg := buffer.New(time.Duration(cfg.Window.MergeSeconds)*time.Second, proc.Handle)
	srv := server.New(cfg, deduper, agg)
	return &App{
		Cfg: cfg,
		srv: srv,
		agg: agg,
		httpSrv: &http.Server{
			Addr:              cfg.Server.Addr,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// loadPrompt 读取分析规则文件；失败或为空时返回空串（LLM 用内置兜底）。
func loadPrompt(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("W! cannot read prompt file %s: %v (use built-in)", path, err)
		return ""
	}
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// Handler 返回 HTTP 路由（供 httptest 使用）。
func (a *App) Handler() http.Handler { return a.srv.Handler() }

// Start 监听并启动 HTTP 服务；监听失败时返回错误（调用方 fail-fast），
// 避免"端口被占但进程活着什么都不做"的僵尸状态。
func (a *App) Start() error {
	ln, err := net.Listen("tcp", a.Cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.Cfg.Server.Addr, err)
	}
	log.Printf("I! linkage listening on %s", a.Cfg.Server.Addr)
	go func() {
		if err := a.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("E! server: %v", err)
		}
	}()
	return nil
}

// Stop 优雅停机：先关闭 HTTP（等待在途请求），再补发聚合缓冲中的待分析事件。
func (a *App) Stop(ctx context.Context) {
	_ = a.httpSrv.Shutdown(ctx)
	a.agg.Drain()
}
