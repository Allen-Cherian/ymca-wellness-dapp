// Entry point for the ymca-wellness-dapp dApp server.
//
// Wires: config -> db pool -> rubix-backed service -> per-admin queue ->
// gin server. Shuts down cleanly on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ymca-wellness-dapp/internal/auth"
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

	keys, err := auth.LoadKeys(cfg.Env.JWTPrivateKeyPath, cfg.Env.JWTPublicKeyPath)
	if err != nil {
		log.Fatalf("auth.LoadKeys: %v", err)
	}

	if err := seedBootstrapOperator(ctx, cfg); err != nil {
		log.Fatalf("bootstrap operator: %v", err)
	}

	svc := service.New(cfg)
	// Worker job timeout = 2x HTTP timeout to allow for Sign() blocking.
	procTimeout := time.Duration(cfg.Env.RubixHTTPTimeoutSecond*2) * time.Second
	qm := queue.NewManager(svc, cfg.Env.QueueBufferSize, procTimeout)

	srv := server.New(cfg, svc, qm, keys)

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

// seedBootstrapOperator creates a single operator from BOOTSTRAP_EMAIL /
// BOOTSTRAP_PASSWORD if the auth_users table is empty. Idempotent: a
// non-empty table short-circuits without touching env vars. Missing env
// on first boot is non-fatal but logs loudly — operators must set them
// and restart before anyone can log in.
func seedBootstrapOperator(ctx context.Context, cfg *config.AppConfig) error {
	count, err := database.CountAuthUsers(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if cfg.Env.BootstrapEmail == "" || cfg.Env.BootstrapPassword == "" {
		log.Printf("warning: auth_users is empty and BOOTSTRAP_EMAIL/BOOTSTRAP_PASSWORD are unset; nobody can log in until you set them and restart")
		return nil
	}
	hash, err := auth.HashPassword(cfg.Env.BootstrapPassword)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordTooShort) {
			return err
		}
		return err
	}
	u, err := database.CreateAuthUser(ctx, cfg.Env.BootstrapEmail, hash, database.RoleOperator)
	if err != nil {
		return err
	}
	log.Printf("bootstrap operator created: %s (id=%s)", u.Email, u.ID)
	return nil
}
