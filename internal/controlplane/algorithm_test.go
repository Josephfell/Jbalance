package controlplane

import (
	"path/filepath"
	"testing"
)

func TestAlgorithmStore_DefaultsToRoundRobin(t *testing.T) {
	s := NewAlgorithmStore("")
	if got := s.Get("g1"); got != AlgorithmRoundRobin {
		t.Errorf("expected default algorithm round_robin, got %s", got)
	}
}

func TestAlgorithmStore_SetAndGet(t *testing.T) {
	s := NewAlgorithmStore("")
	if err := s.Set("g1", AlgorithmLeastConnections); err != nil {
		t.Fatalf("Set() error: %v", err)
	}
	if got := s.Get("g1"); got != AlgorithmLeastConnections {
		t.Errorf("expected least_connections, got %s", got)
	}
}

func TestAlgorithmStore_SetRejectsInvalidAlgorithm(t *testing.T) {
	s := NewAlgorithmStore("")
	err := s.Set("g1", Algorithm("not-a-real-algorithm"))
	if err == nil {
		t.Fatal("expected an error for an invalid algorithm")
	}
	// A rejected Set must not have taken effect.
	if got := s.Get("g1"); got != AlgorithmRoundRobin {
		t.Errorf("expected g1 to remain at the default after a rejected Set, got %s", got)
	}
}

func TestAlgorithmStore_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "algorithms.json")

	s1 := NewAlgorithmStore(path)
	if err := s1.Set("g1", AlgorithmRandom); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	s2 := NewAlgorithmStore(path)
	if got := s2.Get("g1"); got != AlgorithmRandom {
		t.Errorf("expected reloaded algorithm random, got %s", got)
	}
}

func TestAlgorithmStore_MissingFileDefaultsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewAlgorithmStore(path)
	if got := s.Get("g1"); got != AlgorithmRoundRobin {
		t.Errorf("expected default round_robin when the file doesn't exist, got %s", got)
	}
}

func TestIsValidAlgorithm(t *testing.T) {
	cases := []struct {
		algo Algorithm
		want bool
	}{
		{AlgorithmRoundRobin, true},
		{AlgorithmLeastConnections, true},
		{AlgorithmRandom, true},
		{Algorithm("bogus"), false},
		{Algorithm(""), false},
	}
	for _, tc := range cases {
		if got := IsValidAlgorithm(tc.algo); got != tc.want {
			t.Errorf("IsValidAlgorithm(%q) = %v, want %v", tc.algo, got, tc.want)
		}
	}
}
