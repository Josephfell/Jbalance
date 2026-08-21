package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Override is a manual adjustment applied on top of whatever a
// pool.Provider reports for one backend address, set via the admin web
// UI. A nil Weight means "don't override the provider's weight"; Drained
// means "exclude this backend entirely, regardless of what the provider
// reports" — useful for taking an instance out of rotation for
// maintenance without waiting on (or fighting) the provider's own view.
type Override struct {
	Weight  *int32 `json:"weight,omitempty"`
	Drained bool   `json:"drained"`
}

// OverrideStore holds manual per-backend overrides, keyed by group then
// address, persisted to a single local JSON file — no external database,
// consistent with the rest of this tool's admin state. An empty path
// means in-memory only (used in tests).
type OverrideStore struct {
	path string
	mu   sync.RWMutex
	data map[string]map[string]Override // group -> address -> Override
}

// NewOverrideStore loads overrides from path if it exists, or starts
// empty if it doesn't (or the file is corrupt — overrides are a
// convenience feature, not worth failing startup over).
func NewOverrideStore(path string) *OverrideStore {
	s := &OverrideStore{path: path, data: make(map[string]map[string]Override)}

	if path == "" {
		return s
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var loaded map[string]map[string]Override
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			s.data = loaded
		}
	}

	return s
}

// GroupOverrides returns a defensive copy of every override currently set
// for the given group.
func (s *OverrideStore) GroupOverrides(group string) map[string]Override {
	s.mu.RLock()
	defer s.mu.RUnlock()

	src := s.data[group]
	out := make(map[string]Override, len(src))
	for addr, ov := range src {
		out[addr] = ov
	}
	return out
}

// Set stores an override for the given group/address and persists it.
func (s *OverrideStore) Set(group, address string, weight *int32, drained bool) error {
	s.mu.Lock()
	if s.data[group] == nil {
		s.data[group] = make(map[string]Override)
	}
	s.data[group][address] = Override{Weight: weight, Drained: drained}
	snapshot := s.cloneLocked()
	s.mu.Unlock()

	return s.persist(snapshot)
}

// Clear removes any override for the given group/address and persists the
// result. A no-op (no error) if no override existed.
func (s *OverrideStore) Clear(group, address string) error {
	s.mu.Lock()
	if s.data[group] != nil {
		delete(s.data[group], address)
		if len(s.data[group]) == 0 {
			delete(s.data, group)
		}
	}
	snapshot := s.cloneLocked()
	s.mu.Unlock()

	return s.persist(snapshot)
}

// cloneLocked returns a deep copy of s.data. Callers must hold s.mu.
func (s *OverrideStore) cloneLocked() map[string]map[string]Override {
	out := make(map[string]map[string]Override, len(s.data))
	for group, addrs := range s.data {
		inner := make(map[string]Override, len(addrs))
		for addr, ov := range addrs {
			inner[addr] = ov
		}
		out[group] = inner
	}
	return out
}

// persist writes data to s.path atomically (temp file + rename). A no-op
// if the store was created with an empty path (in-memory only).
func (s *OverrideStore) persist(data map[string]map[string]Override) error {
	if s.path == "" {
		return nil
	}

	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal overrides: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlplane: failed to create overrides directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".overrides-*.tmp")
	if err != nil {
		return fmt.Errorf("controlplane: failed to create overrides temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to write overrides temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to set overrides file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("controlplane: failed to close overrides temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("controlplane: failed to persist overrides: %w", err)
	}
	return nil
}
