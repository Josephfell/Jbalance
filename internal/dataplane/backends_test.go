package dataplane

import (
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestBackendList_RoundRobin(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:   "test",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
			{Address: "c:1", Weight: 1},
		},
	})

	got := make([]string, 6)
	for i := range got {
		addr, ok := bl.Next()
		if !ok {
			t.Fatalf("expected a backend, got none at index %d", i)
		}
		got[i] = addr
	}

	want := []string{"a:1", "b:1", "c:1", "a:1", "b:1", "c:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %s, want %s (full: %v)", i, got[i], want[i], got)
			break
		}
	}
}

func TestBackendList_WeightedDistribution(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:   "test",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 3},
			{Address: "b:1", Weight: 1},
		},
	})

	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		addr, _ := bl.Next()
		counts[addr]++
	}

	// Over 8 selections with weights 3:1, expect 6 for a and 2 for b.
	if counts["a:1"] != 6 || counts["b:1"] != 2 {
		t.Errorf("unexpected weighted distribution: %v", counts)
	}
}

func TestBackendList_NoBackends(t *testing.T) {
	bl := NewBackendList()
	if _, ok := bl.Next(); ok {
		t.Error("expected no backend to be available before any Update")
	}
}

func TestBackendList_IgnoresStaleVersions(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  5,
		Backends: []*pb.Backend{{Address: "current:1", Weight: 1}},
	})

	// A lower version should be ignored — simulates an out-of-order
	// delivery or a reconnect that briefly replays an old update.
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  3,
		Backends: []*pb.Backend{{Address: "stale:1", Weight: 1}},
	})

	addr, ok := bl.Next()
	if !ok || addr != "current:1" {
		t.Errorf("expected stale update to be ignored, got addr=%s ok=%v", addr, ok)
	}

	if bl.Version() != 5 {
		t.Errorf("expected version to remain 5, got %d", bl.Version())
	}
}

func TestBackendList_UnhealthyBackendsExcludedFromRotation(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:   "test",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})

	bl.SetHealth("a:1", false)

	for i := 0; i < 4; i++ {
		addr, ok := bl.Next()
		if !ok {
			t.Fatal("expected a healthy backend to be available")
		}
		if addr != "b:1" {
			t.Errorf("expected only healthy backend b:1 to be selected, got %s", addr)
		}
	}
}

func TestBackendList_AllUnhealthyReturnsNoBackend(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}},
	})
	bl.SetHealth("a:1", false)

	if _, ok := bl.Next(); ok {
		t.Error("expected no backend to be available when all are unhealthy")
	}
}

func TestBackendList_HealthPreservedAcrossUpdate(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}},
	})
	bl.SetHealth("a:1", false)

	// A new control-plane update that still includes "a:1" should not
	// silently reset its health back to healthy.
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  2,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}, {Address: "c:1", Weight: 1}},
	})

	if bl.HealthyLen() != 2 { // b:1 and the newly-added, optimistically-healthy c:1
		t.Errorf("expected 2 healthy backends after update, got %d", bl.HealthyLen())
	}

	for i := 0; i < 6; i++ {
		addr, _ := bl.Next()
		if addr == "a:1" {
			t.Error("expected previously-unhealthy backend a:1 to remain excluded after an update")
		}
	}
}

func TestBackendList_NewBackendStartsOptimisticallyHealthy(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}},
	})

	if bl.HealthyLen() != 1 {
		t.Errorf("expected a newly-added backend to start healthy, got healthy count %d", bl.HealthyLen())
	}
}

func TestBackendList_SetHealthOnUnknownAddressIsNoop(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}},
	})

	bl.SetHealth("does-not-exist:1", false)

	if bl.HealthyLen() != 1 {
		t.Errorf("expected SetHealth on an unknown address to be a no-op, got healthy count %d", bl.HealthyLen())
	}
}

func TestBackendList_ZeroWeightTreatedAsOne(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:   "test",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 0},
			{Address: "b:1", Weight: 0},
		},
	})

	counts := map[string]int{}
	for i := 0; i < 4; i++ {
		addr, _ := bl.Next()
		counts[addr]++
	}

	if counts["a:1"] != 2 || counts["b:1"] != 2 {
		t.Errorf("expected equal distribution for zero-weight backends, got %v", counts)
	}
}

func TestBackendList_LeastConnections_PicksLeastLoaded(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "least_connections",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})

	// First selection is a tie (both at 0 active conns); take whichever
	// comes back and verify the *other* one is picked next, since the
	// first pick's count is now higher.
	first, ok := bl.Next()
	if !ok {
		t.Fatal("expected a backend")
	}
	second, ok := bl.Next()
	if !ok {
		t.Fatal("expected a backend")
	}
	if first == second {
		t.Errorf("expected least_connections to pick the other backend once the first has an active connection, got %s twice", first)
	}

	// Releasing the busier one should make it eligible again.
	bl.Release(first)
	third, ok := bl.Next()
	if !ok {
		t.Fatal("expected a backend")
	}
	if third != first {
		t.Errorf("expected releasing %s to make it least-loaded again, got %s", first, third)
	}
}

func TestBackendList_LeastConnections_RespectsWeight(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "least_connections",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 2}, // can carry 2x the load of b before being "equally loaded"
			{Address: "b:1", Weight: 1},
		},
	})

	counts := map[string]int{}
	for i := 0; i < 3; i++ {
		addr, ok := bl.Next()
		if !ok {
			t.Fatal("expected a backend")
		}
		counts[addr]++
	}

	// load = activeConns/weight: a:1 should absorb 2 of the first 3
	// selections before its load (1.0) matches b:1's load after its 1st
	// selection (1.0).
	if counts["a:1"] != 2 || counts["b:1"] != 1 {
		t.Errorf("expected weighted least_connections distribution a=2,b=1, got %v", counts)
	}
}

func TestBackendList_Release_DoesNotGoNegative(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "least_connections",
		Backends:  []*pb.Backend{{Address: "a:1", Weight: 1}},
	})

	// Release without a matching Next — must not underflow.
	bl.Release("a:1")
	bl.Release("a:1")

	addr, ok := bl.Next()
	if !ok || addr != "a:1" {
		t.Fatalf("expected a:1 still selectable after over-releasing, got addr=%s ok=%v", addr, ok)
	}
}

func TestBackendList_Release_UnknownAddressIsNoop(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}},
	})

	// Must not panic or affect anything.
	bl.Release("does-not-exist:1")

	if _, ok := bl.Next(); !ok {
		t.Error("expected a:1 to remain selectable")
	}
}

func TestBackendList_Random_OnlySelectsHealthyKnownBackends(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "random",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})

	known := map[string]bool{"a:1": true, "b:1": true}
	for i := 0; i < 20; i++ {
		addr, ok := bl.Next()
		if !ok || !known[addr] {
			t.Fatalf("unexpected selection from random algorithm: addr=%s ok=%v", addr, ok)
		}
	}
}

func TestBackendList_UnrecognisedAlgorithmDefaultsToRoundRobin(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "some-future-algorithm-this-build-does-not-know",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})

	if bl.Algorithm() != AlgorithmRoundRobin {
		t.Errorf("expected an unrecognised algorithm to fall back to round_robin, got %s", bl.Algorithm())
	}

	got := []string{}
	for i := 0; i < 4; i++ {
		addr, _ := bl.Next()
		got = append(got, addr)
	}
	want := []string{"a:1", "b:1", "a:1", "b:1"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("expected round-robin fallback ordering %v, got %v", want, got)
			break
		}
	}
}

func TestBackendList_AlgorithmGetter(t *testing.T) {
	bl := NewBackendList()
	if bl.Algorithm() != AlgorithmRoundRobin {
		t.Errorf("expected default algorithm to be round_robin, got %s", bl.Algorithm())
	}

	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "least_connections",
		Backends:  []*pb.Backend{{Address: "a:1", Weight: 1}},
	})

	if bl.Algorithm() != AlgorithmLeastConnections {
		t.Errorf("expected algorithm to update to least_connections, got %s", bl.Algorithm())
	}
}

func TestBackendList_ActiveConnsPreservedAcrossUpdate(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   1,
		Algorithm: "least_connections",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})

	// Give a:1 an active connection, then push an update that still
	// includes a:1 — its in-flight count should survive the reconcile.
	busy, ok := bl.Next()
	if !ok {
		t.Fatal("expected a backend")
	}

	bl.Update(&pb.BackendSet{
		Group:     "test",
		Version:   2,
		Algorithm: "least_connections",
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
			{Address: "c:1", Weight: 1},
		},
	})

	// The backend that was busy before the update should still be
	// considered busier than the other pre-existing one, so the next
	// selection should avoid it in favour of the other original backend
	// or the newly-added one (both at 0 active conns).
	next, ok := bl.Next()
	if !ok {
		t.Fatal("expected a backend")
	}
	if next == busy {
		t.Errorf("expected active-connection count for %s to be preserved across update (making it less preferred), but it was selected again immediately", busy)
	}
}

func TestBackendList_Sticky_DefaultDisabled(t *testing.T) {
	bl := NewBackendList()
	if bl.Sticky().Enabled {
		t.Error("expected sticky sessions to default to disabled")
	}
}

func TestBackendList_Sticky_ParsedFromUpdate(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:            "test",
		Version:          1,
		Backends:         []*pb.Backend{{Address: "a:1", Weight: 1}},
		Sticky:           true,
		StickyCookieName: "my_cookie",
		StickyTtlSeconds: 900,
	})

	sc := bl.Sticky()
	if !sc.Enabled {
		t.Error("expected sticky to be enabled")
	}
	if sc.CookieName != "my_cookie" {
		t.Errorf("expected cookie name my_cookie, got %q", sc.CookieName)
	}
	if sc.TTL != 15*time.Minute {
		t.Errorf("expected TTL 15m, got %v", sc.TTL)
	}
}

func TestBackendList_PinTo_HealthyKnownAddress(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{Group: "test", Version: 1, Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}})

	if !bl.PinTo("a:1") {
		t.Error("expected PinTo to succeed for a healthy, known address")
	}
}

func TestBackendList_PinTo_UnhealthyAddressFails(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{Group: "test", Version: 1, Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}})
	bl.SetHealth("a:1", false)

	if bl.PinTo("a:1") {
		t.Error("expected PinTo to fail for an unhealthy address")
	}
}

func TestBackendList_PinTo_UnknownAddressFails(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{Group: "test", Version: 1, Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}})

	if bl.PinTo("does-not-exist:1") {
		t.Error("expected PinTo to fail for an address not in the group")
	}
}

func TestBackendList_PinTo_IncrementsActiveConns(t *testing.T) {
	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group: "test", Version: 1, Algorithm: "least_connections",
		Backends: []*pb.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}},
	})

	bl.PinTo("a:1")
	bl.PinTo("a:1")

	// With a:1 pinned twice, least_connections should now prefer b:1.
	next, ok := bl.Next()
	if !ok || next != "b:1" {
		t.Errorf("expected PinTo to raise a:1's active-connection count enough that least_connections picks b:1 next, got %q", next)
	}
}
