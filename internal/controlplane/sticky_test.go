package controlplane

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStickyStore_DefaultDisabled(t *testing.T) {
	s := NewStickyStore("")
	cfg := s.Get("g1")
	if cfg.Enabled {
		t.Error("expected sticky sessions to default to disabled for an unconfigured group")
	}
}

func TestStickyStore_SetAndGet(t *testing.T) {
	s := NewStickyStore("")
	cfg := StickyConfig{Enabled: true, CookieName: "my_cookie", TTL: 15 * time.Minute}
	if err := s.Set("g1", cfg); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	got := s.Get("g1")
	if !got.Enabled || got.CookieName != "my_cookie" || got.TTL != 15*time.Minute {
		t.Errorf("unexpected config after Set: %+v", got)
	}
}

func TestStickyConfig_EffectiveDefaults(t *testing.T) {
	cfg := StickyConfig{Enabled: true}
	if cfg.effectiveCookieName() != defaultStickyCookieName {
		t.Errorf("expected default cookie name %q, got %q", defaultStickyCookieName, cfg.effectiveCookieName())
	}
	if cfg.effectiveTTL() != defaultStickyTTL {
		t.Errorf("expected default TTL %v, got %v", defaultStickyTTL, cfg.effectiveTTL())
	}
	if cfg.TTLMinutes() != int(defaultStickyTTL.Minutes()) {
		t.Errorf("expected TTLMinutes %d, got %d", int(defaultStickyTTL.Minutes()), cfg.TTLMinutes())
	}
	if cfg.CookieDisplayName() != defaultStickyCookieName {
		t.Errorf("expected CookieDisplayName %q, got %q", defaultStickyCookieName, cfg.CookieDisplayName())
	}
}

func TestStickyConfig_EffectiveOverrides(t *testing.T) {
	cfg := StickyConfig{Enabled: true, CookieName: "custom", TTL: 5 * time.Minute}
	if cfg.effectiveCookieName() != "custom" {
		t.Errorf("expected custom cookie name, got %q", cfg.effectiveCookieName())
	}
	if cfg.effectiveTTL() != 5*time.Minute {
		t.Errorf("expected TTL 5m, got %v", cfg.effectiveTTL())
	}
}

func TestStickyStore_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sticky.json")

	s1 := NewStickyStore(path)
	if err := s1.Set("g1", StickyConfig{Enabled: true, CookieName: "c1", TTL: 10 * time.Minute}); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	s2 := NewStickyStore(path)
	got := s2.Get("g1")
	if !got.Enabled || got.CookieName != "c1" || got.TTL != 10*time.Minute {
		t.Errorf("expected reloaded config to match what was set, got %+v", got)
	}
}

func TestStickyStore_MissingFileDefaultsToDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewStickyStore(path)
	if s.Get("g1").Enabled {
		t.Error("expected an unconfigured store to default to disabled")
	}
}
