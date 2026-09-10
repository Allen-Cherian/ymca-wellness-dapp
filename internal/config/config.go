// Package config loads runtime configuration from .env. Admins are
// sourced from the database (see internal/database/admins.go); this
// package keeps an in-memory map for O(1) lookup, refreshable via
// ReloadAdmins.
package config

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"

	"ymca-wellness-dapp/internal/database"
)

// EnvConfig holds values sourced from environment variables / .env.
type EnvConfig struct {
	ServerPort string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	FTName                 string
	RubixHTTPTimeoutSecond int
	QueueBufferSize        int
	// PayoutMinInterval is the minimum gap between the end of one reward
	// execution and the start of the next one for the SAME admin. Zero
	// disables spacing. See docs/INCIDENT-2026-09-04-chain-fork.md: back-
	// to-back executions on one contract crash the owner node mid-consensus.
	PayoutMinInterval time.Duration
	// PayoutMinIntervalOverrides sets a different gap for specific admins,
	// keyed by admin DID (PAYOUT_MIN_INTERVAL_OVERRIDES="<did>=<ms>,...").
	// An admin absent from the map uses PayoutMinInterval.
	PayoutMinIntervalOverrides map[string]time.Duration

	// Bearer-token auth
	JWTPrivateKeyPath string
	JWTPublicKeyPath  string
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	BootstrapEmail    string
	BootstrapPassword string
}

// AdminConfig describes one admin node the dApp server talks to. It is
// the in-memory shape of a database.Admin; node_host is always
// "http://localhost".
type AdminConfig struct {
	DID      string
	Password string
	NodePort string
}

// NodeHost is hardcoded — the dApp talks to admin nodes on localhost.
const NodeHost = "http://localhost"

// BaseURL returns "http://localhost:<port>" for HTTP requests to this
// admin's node.
func (a AdminConfig) BaseURL() string {
	return fmt.Sprintf("%s:%s", NodeHost, a.NodePort)
}

// AppConfig is the merged, ready-to-use config.
type AppConfig struct {
	Env EnvConfig

	mu         sync.RWMutex
	adminByDID map[string]AdminConfig
}

// Load reads .env into EnvConfig. Admins must be loaded separately via
// ReloadAdmins once the database pool is up.
func Load() (*AppConfig, error) {
	// .env is optional; real env vars win either way.
	_ = godotenv.Load()

	env := EnvConfig{
		ServerPort:             getEnv("SERVER_PORT", "9000"),
		DBHost:                 getEnv("DB_HOST", "localhost"),
		DBPort:                 getEnv("DB_PORT", "5432"),
		DBUser:                 getEnv("DB_USER", "postgres"),
		DBPassword:             getEnv("DB_PASSWORD", "postgres"),
		DBName:                 getEnv("DB_NAME", "ymca_wellness_cafe_v2"),
		DBSSLMode:              getEnv("DB_SSLMODE", "disable"),
		FTName:                 getEnv("FT_NAME", "ytoken"),
		RubixHTTPTimeoutSecond: getEnvInt("RUBIX_HTTP_TIMEOUT_SECONDS", 120),
		QueueBufferSize:        getEnvInt("QUEUE_BUFFER_SIZE", 1000),
		PayoutMinInterval:      time.Duration(getEnvInt("PAYOUT_MIN_INTERVAL_MS", 1000)) * time.Millisecond,

		JWTPrivateKeyPath: getEnv("JWT_PRIVATE_KEY_PATH", "./keys/jwt_private.pem"),
		JWTPublicKeyPath:  getEnv("JWT_PUBLIC_KEY_PATH", "./keys/jwt_public.pem"),
		AccessTokenTTL:    getEnvDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL:   getEnvDuration("REFRESH_TOKEN_TTL", 7*24*time.Hour),
		BootstrapEmail:    getEnv("BOOTSTRAP_EMAIL", ""),
		BootstrapPassword: getEnv("BOOTSTRAP_PASSWORD", ""),
	}

	overrides, err := parseIntervalOverrides(os.Getenv("PAYOUT_MIN_INTERVAL_OVERRIDES"))
	if err != nil {
		return nil, fmt.Errorf("config: PAYOUT_MIN_INTERVAL_OVERRIDES: %w", err)
	}
	env.PayoutMinIntervalOverrides = overrides

	return &AppConfig{
		Env:        env,
		adminByDID: make(map[string]AdminConfig),
	}, nil
}

// parseIntervalOverrides parses "<did>=<ms>,<did>=<ms>" (":" also accepted
// as the separator, whitespace ignored). Empty input yields an empty map.
// A malformed entry is an error so a typo cannot silently drop a
// protection.
func parseIntervalOverrides(raw string) (map[string]time.Duration, error) {
	out := make(map[string]time.Duration)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		sep := strings.LastIndexAny(entry, "=:")
		if sep <= 0 || sep == len(entry)-1 {
			return nil, fmt.Errorf("entry %q must be <admin_did>=<milliseconds>", entry)
		}
		did := strings.TrimSpace(entry[:sep])
		ms, err := strconv.Atoi(strings.TrimSpace(entry[sep+1:]))
		if err != nil || ms < 0 {
			return nil, fmt.Errorf("entry %q: milliseconds must be a non-negative integer", entry)
		}
		out[did] = time.Duration(ms) * time.Millisecond
	}
	return out, nil
}

// ReloadAdmins refreshes the in-memory admin map from the database. Call
// at startup and after any provision operation.
func (c *AppConfig) ReloadAdmins(ctx context.Context) error {
	rows, err := database.ListAdmins(ctx)
	if err != nil {
		return fmt.Errorf("config.ReloadAdmins: %w", err)
	}
	m := make(map[string]AdminConfig, len(rows))
	for _, a := range rows {
		m[a.DID] = AdminConfig{
			DID:      a.DID,
			Password: a.Password,
			NodePort: a.NodePort,
		}
	}
	c.mu.Lock()
	c.adminByDID = m
	c.mu.Unlock()
	return nil
}

// AdminByDID returns the admin record for a DID, or ok=false if unknown.
func (c *AppConfig) AdminByDID(did string) (AdminConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a, ok := c.adminByDID[did]
	return a, ok
}

// AdminCount returns the number of admins currently loaded.
func (c *AppConfig) AdminCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.adminByDID)
}

// DBDSN builds a libpq-style connection string for pgx.
func (c *AppConfig) DBDSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.Env.DBHost, c.Env.DBPort, c.Env.DBUser, c.Env.DBPassword, c.Env.DBName, c.Env.DBSSLMode,
	)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
