// Command linkage 是硬件告警联动诊断服务的入口。
//
// 用法:
//
//	linkage --config /etc/kernel-hw-linkage/config.json
//
// 说明: 当前为单进程（webhook 入口 + 进程内聚合/执行）。
// 设计文档规划的 server/worker 拆分需引入 Redis+asynq 后实现，
// 接口已留好（dedup.Deduper、buffer.Aggregator），届时拆分即可。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kernel-hw-linkage/internal/app"
	"kernel-hw-linkage/internal/config"
)

func main() {
	configPath := flag.String("config", "", "config file path (JSON)")
	addr := flag.String("addr", "", "listen addr (overrides config)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("E! config: %v", err)
	}
	if *addr != "" {
		cfg.Server.Addr = *addr
	}

	a := app.New(cfg)
	if err := a.Start(); err != nil {
		log.Fatalf("E! listen: %v", err)
	}

	// 优雅退出：等待在途请求，并补发聚合缓冲中的待分析事件
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("I! shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	a.Stop(ctx)
}
