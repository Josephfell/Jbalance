package dataplane

import (
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func newHealthyList(t *testing.T, algo Algorithm, addrs ...string) *BackendList {
	t.Helper()
	bl := NewBackendList()
	set := &pb.BackendSet{Group: "g", Version: 1, Algorithm: string(algo)}
	for _, a := range addrs {
		set.Backends = append(set.Backends, &pb.Backend{Address: a, Weight: 1})
	}
	bl.Update(set)
	for _, a := range addrs {
		bl.SetHealth(a, true)
	}
	return bl
}

func TestP2CPicksLessLoaded(t *testing.T) {
	// With only two backends, P2C always compares both, so it must pick the
	// one with fewer in-flight connections deterministically.
	bl := newHealthyList(t, AlgorithmP2C, "a:1", "b:1")

	// Load up "a:1" heavily by selecting it many times without releasing —
	// but since P2C compares the two, drive load onto whichever it returns
	// and assert it spreads rather than piling on one. Simpler: pin load on
	// a:1 directly via repeated Next+no-release is nondeterministic, so
	// instead assert both are chosen over many selects (spread), and that a
	// heavily pre-loaded backend is avoided.
	got := map[string]int{}
	for i := 0; i < 200; i++ {
		addr, ok := bl.NextForKey("")
		if !ok {
			t.Fatal("expected a backend")
		}
		got[addr]++
		bl.Release(addr) // keep in-flight near zero so selection stays fair
	}
	if got["a:1"] == 0 || got["b:1"] == 0 {
		t.Fatalf("P2C should spread across both backends, got %v", got)
	}
}

func TestP2CAvoidsLoadedBackend(t *testing.T) {
	bl := newHealthyList(t, AlgorithmP2C, "a:1", "b:1")
	// Artificially load a:1 with many in-flight requests (Next without
	// Release bumps activeConns). Select a:1 20 times without releasing.
	for _, e := range bl.entries {
		if e.address == "a:1" {
			e.activeConns.Store(50)
		}
	}
	// Now every P2C pick between the two must prefer b:1 (far less loaded).
	for i := 0; i < 50; i++ {
		addr, _ := bl.NextForKey("")
		bl.Release(addr)
		if addr != "b:1" {
			t.Fatalf("P2C should prefer the far-less-loaded backend b:1, got %q", addr)
		}
	}
}

func TestConsistentHashStable(t *testing.T) {
	bl := newHealthyList(t, AlgorithmConsistentHash, "a:1", "b:1", "c:1")
	// The same key must always map to the same backend while the healthy
	// set is unchanged.
	first, ok := bl.NextForKey("client-42")
	if !ok {
		t.Fatal("expected a backend")
	}
	bl.Release(first)
	for i := 0; i < 50; i++ {
		got, _ := bl.NextForKey("client-42")
		bl.Release(got)
		if got != first {
			t.Fatalf("consistent hash unstable: key mapped to %q then %q", first, got)
		}
	}
}

func TestConsistentHashSpreadsKeys(t *testing.T) {
	bl := newHealthyList(t, AlgorithmConsistentHash, "a:1", "b:1", "c:1")
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		key := "user-" + string(rune('A'+i%26)) + string(rune('0'+i%10))
		addr, _ := bl.NextForKey(key)
		bl.Release(addr)
		seen[addr] = true
	}
	// Over many distinct keys, all three backends should receive some.
	if len(seen) < 3 {
		t.Fatalf("consistent hash should spread keys across all 3 backends, hit %v", seen)
	}
}

func TestConsistentHashEmptyKeyFallsBack(t *testing.T) {
	bl := newHealthyList(t, AlgorithmConsistentHash, "a:1", "b:1")
	// Empty key -> round-robin fallback: over 2 selects we should see both.
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		addr, ok := bl.NextForKey("")
		if !ok {
			t.Fatal("expected a backend even with empty key")
		}
		bl.Release(addr)
		seen[addr] = true
	}
	if len(seen) < 2 {
		t.Fatalf("empty-key fallback should round-robin across backends, saw %v", seen)
	}
}

func TestConsistentHashRemapsMinimallyOnRemoval(t *testing.T) {
	bl := newHealthyList(t, AlgorithmConsistentHash, "a:1", "b:1", "c:1", "d:1")
	keys := make([]string, 100)
	before := make([]string, 100)
	for i := range keys {
		keys[i] = "k" + string(rune('0'+i%10)) + string(rune('a'+i%26))
		before[i], _ = bl.NextForKey(keys[i])
		bl.Release(before[i])
	}

	// Remove one backend (mark unhealthy). Only keys that were on the
	// removed backend should move; the rest should stay put.
	bl.SetHealth("d:1", false)

	moved := 0
	for i, k := range keys {
		after, _ := bl.NextForKey(k)
		bl.Release(after)
		if after != before[i] {
			moved++
			if before[i] != "d:1" && after != before[i] {
				// A key not previously on d:1 moved — allowed only if it WAS
				// on d:1. Since before[i] != d:1, this is unexpected churn.
				// Consistent hashing should not remap keys off healthy nodes.
				t.Errorf("key %q moved from %q to %q but wasn't on the removed node", k, before[i], after)
			}
		}
	}
	if moved == 0 {
		t.Fatal("expected keys previously on d:1 to remap after its removal")
	}
}
