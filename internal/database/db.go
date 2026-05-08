// Package database provides pgxpool setup and typed helpers for the v2
// schema defined in migrations/001_init.sql.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the package-level connection pool.
var Pool *pgxpool.Pool

// Connect opens the pool and pings the server.
func Connect(ctx context.Context, dsn string) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("database: parse dsn: %w", err)
	}
	cfg.MaxConns = 100
	cfg.MinConns = 5
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("database: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return fmt.Errorf("database: ping: %w", err)
	}

	Pool = pool
	return nil
}

// Close releases the pool (safe to call multiple times).
func Close() {
	if Pool != nil {
		Pool.Close()
		Pool = nil
	}
}
