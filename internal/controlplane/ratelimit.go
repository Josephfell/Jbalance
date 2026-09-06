package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RateLimitConfig is one group's per-client request rate limit. Enforced
// at the data plane (L7/http mode) as a token bucket keyed on client IP:
// a client exceeding the rate receives HTTP 429.
type RateLimitConfig struct {
	// Enabled turns rate limiting on for the group. When false, RPS/Burst
	// are ignored and no limiting happens.
	Enabled bool `json:"enabled"`
	// RPS is the sustained per-client request rate (requests per second).
	RPS float64 `json:"rps,omitempty"`
	// Burst is the token-bucket depth — how many requests a client may
	// make in a burst above the sustained rate. 0 lets the data plane
	// default it to the RPS (a one-second burst).
	Burst int `json:"burst,omitempty"`
}

// effectiveRPS/effectiveBurst apply defaults so "enabled with nothing
// else set" behaves sensibly: a default rate of 10 rps, and a burst equal
// to the rate when unset.
func (c RateLimitConfig) effectiveRPS() float64 {
	if c.RPS <= 0 {
		return 10
	}
	return c.RPS
}

func (c RateLimitConfig) effectiveBurst() int {
	if c.Burst <= 0 {
		b := int(c.effectiveRPS())
		if b < 1 {
			b = 1
		}
		return b
	}
	return c.Burst
}

// RPSDisplay and BurstDisplay expose the effective values for the admin
// UI form (template-friendly, since the effective* helpers are
// unexported).
func (c RateLimitConfig) RPSDisplay() float64 { return c.effectiveRPS() }
func (c RateLimitConfig) BurstDisplay() int   { return c.effectiveBurst() }

// RateLimitStore holds per-group rate-limit configuration, persisted to a
// single local JSON file — no external database, same pattern as
// StickyStore/AlgorithmStore/OverrideStore. Groups with no explicit entry
// have rate limiting disabled.
type RateLimitStore struct {
	path string
	mu   sync.RWMutex
	data map[string]RateLimitConfig // group -> config
}

// NewRateLimitStore loads rate-limit configuration from path if it
// exists, or starts empty (rate limiting disabled everywhere) if it
// doesn't.
func NewRateLimitStore(path string) *RateLimitStore {
	s := &RateLimitStore{path: path, data: make(map[string]RateLimitConfig)}
	if path == "" {
		return s
	}
	// path is an operator-provided config-file path (a startup flag), not
	// attacker-controlled input. #nosec G304
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is an operator-configured file, not user input
	if err == nil {
		var loaded map[string]RateLimitConfig
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			s.data = loaded
		}
	}
	return s
}

// Get returns group's rate-limit configuration, or a zero-value
// (disabled) config if none has been explicitly set.
func (s *RateLimitStore) Get(group string) RateLimitConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[group]
}

// Set stores group's rate-limit configuration and persists it.
func (s *RateLimitStore) Set(group string, cfg RateLimitConfig) error {
	s.mu.Lock()
	s.data[group] = cfg
	snapshot := make(map[string]RateLimitConfig, len(s.data))
	for k, v := range s.data {
		snapshot[k] = v
	}
	s.mu.Unlock()
	return s.persist(snapshot)
}

func (s *RateLimitStore) persist(data map[string]RateLimitConfig) error {
	if s.path == "" {
		return nil
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal rate-limit config: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlplane: failed to create rate-limit config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ratelimit-*.tmp")
	if err != nil {
		return fmt.Errorf("controlplane: failed to create rate-limit config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to write rate-limit config temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to set rate-limit config file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("controlplane: failed to close rate-limit config temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("controlplane: failed to persist rate-limit config: %w", err)
	}
	return nil
}
