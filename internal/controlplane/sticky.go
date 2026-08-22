package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// defaultStickyCookieName and defaultStickyTTL are applied when a group's
// StickyConfig doesn't specify them explicitly — the admin UI lets an
// operator enable sticky sessions without having to think about cookie
// naming or TTL up front.
const (
	defaultStickyCookieName = "jb_affinity"
	defaultStickyTTL        = 30 * time.Minute
)

// StickyConfig is one group's session-affinity configuration.
type StickyConfig struct {
	Enabled    bool          `json:"enabled"`
	CookieName string        `json:"cookieName,omitempty"`
	TTL        time.Duration `json:"ttl,omitempty"`
}

// effectiveCookieName and effectiveTTL apply the defaults documented on
// StickyConfig — callers (snapshotToBackendSet, the admin UI) should
// always go through these rather than reading CookieName/TTL directly,
// so "sticky enabled with nothing else set" behaves sensibly everywhere.
func (c StickyConfig) effectiveCookieName() string {
	if c.CookieName == "" {
		return defaultStickyCookieName
	}
	return c.CookieName
}

func (c StickyConfig) effectiveTTL() time.Duration {
	if c.TTL <= 0 {
		return defaultStickyTTL
	}
	return c.TTL
}

// TTLMinutes returns the effective TTL in whole minutes, for display in
// the admin UI's form (which edits TTL as a plain "minutes" number field
// rather than exposing time.Duration's string syntax).
func (c StickyConfig) TTLMinutes() int {
	return int(c.effectiveTTL().Minutes())
}

// CookieDisplayName returns the effective cookie name for display in the
// admin UI — the same default-applying logic effectiveCookieName uses,
// exported under a template-friendly name since effectiveCookieName is
// unexported.
func (c StickyConfig) CookieDisplayName() string {
	return c.effectiveCookieName()
}

// StickyStore holds per-group sticky-session configuration, persisted to
// a single local JSON file — no external database, same pattern as
// AlgorithmStore and OverrideStore. Groups with no explicit entry have
// sticky sessions disabled.
type StickyStore struct {
	path string
	mu   sync.RWMutex
	data map[string]StickyConfig // group -> config
}

// NewStickyStore loads sticky configuration from path if it exists, or
// starts empty (sticky sessions disabled everywhere) if it doesn't.
func NewStickyStore(path string) *StickyStore {
	s := &StickyStore{path: path, data: make(map[string]StickyConfig)}

	if path == "" {
		return s
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var loaded map[string]StickyConfig
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			s.data = loaded
		}
	}

	return s
}

// Get returns group's sticky configuration, or a zero-value (disabled)
// StickyConfig if none has been explicitly set.
func (s *StickyStore) Get(group string) StickyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[group]
}

// Set stores group's sticky configuration and persists it.
func (s *StickyStore) Set(group string, cfg StickyConfig) error {
	s.mu.Lock()
	s.data[group] = cfg
	snapshot := make(map[string]StickyConfig, len(s.data))
	for k, v := range s.data {
		snapshot[k] = v
	}
	s.mu.Unlock()

	return s.persist(snapshot)
}

func (s *StickyStore) persist(data map[string]StickyConfig) error {
	if s.path == "" {
		return nil
	}

	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal sticky session config: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlplane: failed to create sticky session config directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".sticky-*.tmp")
	if err != nil {
		return fmt.Errorf("controlplane: failed to create sticky session config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to write sticky session config temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to set sticky session config file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("controlplane: failed to close sticky session config temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("controlplane: failed to persist sticky session config: %w", err)
	}
	return nil
}
