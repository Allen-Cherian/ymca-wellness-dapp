// Entry point for the ymca-wellness-dapp dApp server.
//
// Wires: config -> db pool -> rubix-backed service -> per-admin queue ->
// gin server. Shuts down cleanly on SIGINT/SIGTERM.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ymca-wellness-dapp/internal/config"
	"ymca-wellness-dapp/internal/database"
	"ymca-wellness-dapp/internal/queue"
	"ymca-wellness-dapp/internal/server"
	"ymca-wellness-dapp/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config.Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := database.Connect(ctx, cfg.DBDSN()); err != nil {
		log.Fatalf("database.Connect: %v", err)
	}
	defer database.Close()

	// Load admins from DB. Empty is OK — operator must POST /api/admins/setup.
	if err := cfg.ReloadAdmins(ctx); err != nil {
		log.Fatalf("config.ReloadAdmins: %v", err)
	}
	if cfg.AdminCount() == 0 {
		log.Printf("warning: no admins configured; provision via POST /api/admins/setup")
	}

	svc := service.New(cfg)
	// Worker job timeout = 2x HTTP timeout to allow for Sign() blocking.
	procTimeout := time.Duration(cfg.Env.RubixHTTPTimeoutSecond*2) * time.Second
	qm := queue.NewManager(svc, cfg.Env.QueueBufferSize, procTimeout)

	srv := server.New(cfg, svc, qm)

	go func() {
		log.Printf("ymca-wellness-dapp listening on :%s (admins=%d, queue_buf=%d)",
			cfg.Env.ServerPort, cfg.AdminCount(), cfg.Env.QueueBufferSize)
		if err := srv.Run(); err != nil {
			log.Fatalf("server.Run: %v", err)
		}
	}()

	// Wait for shutdown signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutdown: draining...")

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	<-drainCtx.Done()
	log.Println("shutdown: done")
}
