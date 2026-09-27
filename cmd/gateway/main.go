package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lechandonga/resilient-edge-gateway-go/internal/config"
	"github.com/lechandonga/resilient-edge-gateway-go/internal/gateway"
)

func main() {
	configPath := flag.String("config", "configs/config.json", "path to gateway config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	gw, err := gateway.New(cfg)
	if err != nil {
		log.Fatalf("build gateway failed: %v", err)
	}
	gw.StartHealthChecks()
	defer gw.StopHealthChecks()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("[gateway] listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gateway server error: %v", err)
		}
	}()

	// 优雅退出：收到中断信号后停止接收新连接，等待在途请求结束。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("[gateway] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[gateway] forced shutdown: %v", err)
	}
}
