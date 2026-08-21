package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Algorithm identifies a load-balancing algorithm a data plane should use
// when selecting among a group's backends.
type Algorithm string

const (
	AlgorithmRoundRobin       Algorithm = "round_robin"
	AlgorithmLeastConnections Algorithm = "least_connections"
	AlgorithmRandom           Algorithm = "random"
)

// ValidAlgorithms lists every algorithm the data plane knows how to
// implement, in the order they should be presented in the admin UI.
var ValidAlgorithms = []Algorithm{AlgorithmRoundRobin, AlgorithmLeastConnections, AlgorithmRandom}

// IsValidAlgorithm reports whether a is one of ValidAlgorithms.
func IsValidAlgorithm(a Algorithm) bool {
	for _, v := range ValidAlgorithms {
		if v == a {
			return true
		}
	}
	return false
}

// AlgorithmStore holds the selected load-balancing algorithm per group,
// persisted to a single local JSON file — no external database. Groups
// with no explicit entry default to AlgorithmRoundRobin. An empty path
// means in-memory only (used in tests).
type AlgorithmStore struct {
	path string
	mu   sync.RWMutex
	data map[string]Algorithm // group -> algorithm
}

// NewAlgorithmStore loads algorithm selections from path if it exists, or
// starts empty (everything defaults to round robin) if it doesn't.
func NewAlgorithmStore(path string) *AlgorithmStore {
	s := &AlgorithmStore{path: path, data: make(map[string]Algorithm)}

	if path == "" {
		return s
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var loaded map[string]Algorithm
		if jsonErr := json.Unmarshal(data, &loaded); jsonErr == nil {
			s.data = loaded
		}
	}

	return s
}

// Get returns the selected algorithm for group, defaulting to
// AlgorithmRoundRobin if none has been explicitly set.
func (s *AlgorithmStore) Get(group string) Algorithm {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.data[group]; ok && IsValidAlgorithm(a) {
		return a
	}
	return AlgorithmRoundRobin
}

// Set stores the selected algorithm for group and persists it. Returns an
// error if algorithm is not one of ValidAlgorithms.
func (s *AlgorithmStore) Set(group string, algorithm Algorithm) error {
	if !IsValidAlgorithm(algorithm) {
		return fmt.Errorf("controlplane: unknown load-balancing algorithm %q", algorithm)
	}

	s.mu.Lock()
	s.data[group] = algorithm
	snapshot := make(map[string]Algorithm, len(s.data))
	for k, v := range s.data {
		snapshot[k] = v
	}
	s.mu.Unlock()

	return s.persist(snapshot)
}

func (s *AlgorithmStore) persist(data map[string]Algorithm) error {
	if s.path == "" {
		return nil
	}

	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("controlplane: failed to marshal algorithm selections: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlplane: failed to create algorithm store directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".algorithm-*.tmp")
	if err != nil {
		return fmt.Errorf("controlplane: failed to create algorithm store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to write algorithm store temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("controlplane: failed to set algorithm store file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("controlplane: failed to close algorithm store temp file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("controlplane: failed to persist algorithm store: %w", err)
	}
	return nil
}
