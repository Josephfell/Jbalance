package controlplane

import (
	"path/filepath"
	"testing"

	"github.com/Josephfell/Jbalance/internal/pool"
)

func TestRateLimitStore_DefaultDisabled(t *testing.T) {
	s := NewRateLimitStore("")
	if s.Get("g1").Enabled {
		t.Error("expected rate limiting to default to disabled for an unconfigured group")
	}
}

func TestRateLimitStore_SetAndGet(t *testing.T) {
	s := NewRateLimitStore("")
	cfg := RateLimitConfig{Enabled: true, RPS: 25, Burst: 40}
	if err := s.Set("g1", cfg); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	got := s.Get("g1")
	if !got.Enabled || got.RPS != 25 || got.Burst != 40 {
		t.Errorf("unexpected config after Set: %+v", got)
	}
}

func TestRateLimitConfig_EffectiveDefaults(t *testing.T) {
	cfg := RateLimitConfig{Enabled: true}
	if cfg.effectiveRPS() != 10 {
		t.Errorf("expected default rps 10, got %v", cfg.effectiveRPS())
	}
	if cfg.effectiveBurst() != 10 {
		t.Errorf("expected default burst = rps (10), got %d", cfg.effectiveBurst())
	}

	cfg2 := RateLimitConfig{Enabled: true, RPS: 3}
	if cfg2.effectiveBurst() != 3 {
		t.Errorf("expected burst to default to rps (3), got %d", cfg2.effectiveBurst())
	}
}

func TestRateLimitStore_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ratelimit.json")
	s1 := NewRateLimitStore(path)
	if err := s1.Set("g1", RateLimitConfig{Enabled: true, RPS: 7, Burst: 14}); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	s2 := NewRateLimitStore(path)
	got := s2.Get("g1")
	if !got.Enabled || got.RPS != 7 || got.Burst != 14 {
		t.Errorf("expected reloaded config to match, got %+v", got)
	}
}

func TestSnapshotToBackendSet_PopulatesRateLimit(t *testing.T) {
	snap := pool.Snapshot{Backends: []pool.Backend{{Address: "a:1", Weight: 1}}}

	// Enabled: fields populated with effective values.
	bs := snapshotToBackendSet("g1", snap, nil, AlgorithmRoundRobin, StickyConfig{},
		RateLimitConfig{Enabled: true, RPS: 12, Burst: 20})
	if bs.RateLimitRps != 12 {
		t.Errorf("RateLimitRps = %v, want 12", bs.RateLimitRps)
	}
	if bs.RateLimitBurst != 20 {
		t.Errorf("RateLimitBurst = %v, want 20", bs.RateLimitBurst)
	}

	// Disabled: zeroed, so the data plane treats it as off.
	bs = snapshotToBackendSet("g1", snap, nil, AlgorithmRoundRobin, StickyConfig{},
		RateLimitConfig{Enabled: false, RPS: 12})
	if bs.RateLimitRps != 0 {
		t.Errorf("disabled RateLimitRps = %v, want 0", bs.RateLimitRps)
	}
}
