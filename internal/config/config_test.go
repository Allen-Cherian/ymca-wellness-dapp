package config

import (
	"testing"
	"time"
)

func TestParseIntervalOverrides(t *testing.T) {
	const did4 = "bafybmid3lonah2dsayt644ptecq6iiyuxmlavstpyoeb3qu6excaxwz7oq"
	got, err := parseIntervalOverrides(" " + did4 + "=5000 , bafybmiother:250,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[did4] != 5*time.Second || got["bafybmiother"] != 250*time.Millisecond {
		t.Errorf("got %v", got)
	}

	empty, err := parseIntervalOverrides("")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty: %v %v", empty, err)
	}

	for _, bad := range []string{"bafybmiabc", "=5000", "bafybmiabc=", "bafybmiabc=five", "bafybmiabc=-1"} {
		if _, err := parseIntervalOverrides(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestLoadRejectsBadOverrides(t *testing.T) {
	t.Setenv("PAYOUT_MIN_INTERVAL_OVERRIDES", "bafybmiabc=oops")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail on a malformed override")
	}
	t.Setenv("PAYOUT_MIN_INTERVAL_OVERRIDES", "bafybmiabc=5000")
	cfg, err := Load()
	if err != nil || cfg.Env.PayoutMinIntervalOverrides["bafybmiabc"] != 5*time.Second {
		t.Fatalf("Load: %v %v", cfg, err)
	}
}

func TestLoadWithoutOverridesUsesDefaults(t *testing.T) {
	t.Setenv("PAYOUT_MIN_INTERVAL_OVERRIDES", "")
	t.Setenv("PAYOUT_MIN_INTERVAL_MS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Env.PayoutMinIntervalOverrides) != 0 || cfg.Env.PayoutMinInterval != time.Second {
		t.Fatalf("defaults: overrides=%v interval=%s", cfg.Env.PayoutMinIntervalOverrides, cfg.Env.PayoutMinInterval)
	}
}
