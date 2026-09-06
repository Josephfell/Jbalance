package dataplane

import (
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestRouteTable_SplitDistributesByWeight(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix: "/",
				Split: []*pb.RouteTarget{
					{Group: "stable", Weight: 90},
					{Group: "canary", Weight: 10},
				},
			},
		},
	})

	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		counts[rt.Resolve("", "/anything", "GET")]++
	}

	if counts["stable"]+counts["canary"] != n {
		t.Fatalf("split resolved to unexpected groups: %v", counts)
	}
	// canary should land near 10% (2000). Allow a generous band for the
	// PRNG so the test isn't flaky.
	canaryPct := float64(counts["canary"]) / float64(n) * 100
	if canaryPct < 6 || canaryPct > 14 {
		t.Errorf("canary share = %.1f%%, want ~10%% (counts %v)", canaryPct, counts)
	}
}

func TestRouteTable_SingleSplitTargetIsDeterministic(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/", Split: []*pb.RouteTarget{{Group: "only", Weight: 5}}},
		},
	})
	for i := 0; i < 100; i++ {
		if got := rt.Resolve("", "/", "GET"); got != "only" {
			t.Fatalf("single-target split resolved to %q, want only", got)
		}
	}
}

func TestRouteTable_TargetGroupUsedWhenNoSplit(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes:  []*pb.Route{{PathPrefix: "/api/", TargetGroup: "api-tier"}},
	})
	if got := rt.Resolve("", "/api/x", "GET"); got != "api-tier" {
		t.Errorf("expected target_group api-tier, got %q", got)
	}
}

func TestRouteTable_TargetGroupsIncludesSplitTargets(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/", Split: []*pb.RouteTarget{
				{Group: "stable", Weight: 90},
				{Group: "canary", Weight: 10},
			}},
		},
	})
	groups := rt.TargetGroups()
	seen := map[string]bool{}
	for _, g := range groups {
		seen[g] = true
	}
	for _, want := range []string{"web-tier", "stable", "canary"} {
		if !seen[want] {
			t.Errorf("TargetGroups missing %q; got %v", want, groups)
		}
	}
}
