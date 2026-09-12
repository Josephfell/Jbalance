package dataplane

import (
	"context"
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// TestGroupManager_HealthyLen checks that HealthyLen sums healthy
// backends across every tracked group and drops as backends go unhealthy
// — the signal that backs the /readyz probe.
func TestGroupManager_HealthyLen(t *testing.T) {
	// A control-plane address that is never dialed successfully is fine:
	// Ensure only kicks off background subscribers/health checkers we do
	// not rely on here — we drive the BackendLists directly. The health
	// checker needs a positive interval or its ticker panics, so give it
	// a long one; the checker never actually reaches any backend here.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gm := NewGroupManager(ctx, "127.0.0.1:0", "test", nil, HealthCheckConfig{
		Interval: time.Hour,
		Timeout:  time.Second,
	}, time.Hour)

	if got := gm.HealthyLen(); got != 0 {
		t.Fatalf("empty manager: HealthyLen()=%d, want 0", got)
	}

	web := gm.Ensure("web")
	web.Update(&pb.BackendSet{
		Group:   "web",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})
	api := gm.Ensure("api")
	api.Update(&pb.BackendSet{
		Group:    "api",
		Version:  1,
		Backends: []*pb.Backend{{Address: "c:1", Weight: 1}},
	})

	if got := gm.HealthyLen(); got != 3 {
		t.Fatalf("after populating two groups: HealthyLen()=%d, want 3", got)
	}

	// Mark one backend unhealthy: the aggregate must drop by exactly one.
	web.SetHealth("a:1", false)
	if got := gm.HealthyLen(); got != 2 {
		t.Fatalf("after one unhealthy: HealthyLen()=%d, want 2", got)
	}

	// All backends unhealthy across all groups => zero => not ready.
	web.SetHealth("b:1", false)
	api.SetHealth("c:1", false)
	if got := gm.HealthyLen(); got != 0 {
		t.Fatalf("after all unhealthy: HealthyLen()=%d, want 0", got)
	}
}
