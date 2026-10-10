// Command server runs the bailucode2api panel: OpenAI-compatible gateway
// over a pool of bailucode upstream accounts, with keepalive, scheduled
// check-ins and an admin web UI.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bailucode2api/internal/api"
	"bailucode2api/internal/auth"
	"bailucode2api/internal/config"
	"bailucode2api/internal/store"
)

func main() {
	cfg := config.Get()
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store open failed: %v", err)
	}
	secretPath := filepath.Join(cfg.DataDir, "session.secret")
	authMgr, err := auth.NewWithSecretFile(cfg.AdminUser, cfg.AdminPassword, secretPath)
	if err != nil {
		log.Fatalf("auth init failed: %v", err)
	}

	apiSrv := api.New(cfg, db, authMgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Background workers.
	apiSrv.StartKeepalive(ctx)      // 30min 轻量鉴权探测：保活 + 标记死号
	apiSrv.StartCheckinScheduler(ctx) // 08:00/20:00 Asia/Shanghai 签到

	srv := &http.Server{
		Addr:              "0.0.0.0:" + cfg.Port,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("bailucode2api starting on http://0.0.0.0:%s (upstream %s)", cfg.Port, cfg.UpstreamBase)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	fmt.Println("shutting down...")
	cancel()
	shCtx, shCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	_ = db.Save()
	fmt.Println("shutdown complete")
}
