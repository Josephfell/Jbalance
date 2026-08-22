package dataplane

import (
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestRouteTable_ResolvesToDefaultGroupWhenEmpty(t *testing.T) {
	rt := NewRouteTable("web-tier")
	if got := rt.Resolve("acme.io", "/anything", "GET"); got != "web-tier" {
		t.Errorf("expected empty table to resolve to the default group, got %q", got)
	}
}

func TestRouteTable_FirstMatchWins(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/api/", TargetGroup: "api-tier"},
			{PathPrefix: "/api/checkout/", TargetGroup: "checkout-tier"}, // more specific, but listed second — should never match
		},
	})

	if got := rt.Resolve("", "/api/checkout/session", "POST"); got != "api-tier" {
		t.Errorf("expected the first matching rule to win regardless of specificity, got %q", got)
	}
}

func TestRouteTable_HostAndMethodMatching(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{Host: "api.acme.io", PathPrefix: "/", Methods: []string{"POST"}, TargetGroup: "api-write-tier"},
			{Host: "api.acme.io", PathPrefix: "/", TargetGroup: "api-tier"},
		},
	})

	if got := rt.Resolve("api.acme.io", "/orders", "POST"); got != "api-write-tier" {
		t.Errorf("expected POST to api.acme.io to match the method-restricted rule, got %q", got)
	}
	if got := rt.Resolve("api.acme.io", "/orders", "GET"); got != "api-tier" {
		t.Errorf("expected GET to api.acme.io to fall through to the general rule, got %q", got)
	}
	if got := rt.Resolve("other.acme.io", "/orders", "POST"); got != "web-tier" {
		t.Errorf("expected a non-matching host to fall back to the default group, got %q", got)
	}
}

func TestRouteTable_IgnoresStaleVersions(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{Version: 5, Routes: []*pb.Route{{PathPrefix: "/", TargetGroup: "current-tier"}}})
	rt.Update(&pb.RouteTable{Version: 3, Routes: []*pb.Route{{PathPrefix: "/", TargetGroup: "stale-tier"}}})

	if got := rt.Resolve("", "/", "GET"); got != "current-tier" {
		t.Errorf("expected a stale update to be ignored, got %q", got)
	}
	if rt.Version() != 5 {
		t.Errorf("expected version to remain 5, got %d", rt.Version())
	}
}

func TestRouteTable_Len(t *testing.T) {
	rt := NewRouteTable("web-tier")
	if rt.Len() != 0 {
		t.Errorf("expected 0 routes initially, got %d", rt.Len())
	}
	rt.Update(&pb.RouteTable{Version: 1, Routes: []*pb.Route{
		{PathPrefix: "/a/", TargetGroup: "a"},
		{PathPrefix: "/b/", TargetGroup: "b"},
	}})
	if rt.Len() != 2 {
		t.Errorf("expected 2 routes, got %d", rt.Len())
	}
}

func TestRouteTable_TargetGroups_IncludesDefaultAndDeduplicates(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{Version: 1, Routes: []*pb.Route{
		{PathPrefix: "/api/", TargetGroup: "api-tier"},
		{PathPrefix: "/api/v2/", TargetGroup: "api-tier"}, // same target as above
		{PathPrefix: "/static/", TargetGroup: "static-edge"},
	}})

	got := rt.TargetGroups()
	want := map[string]bool{"web-tier": true, "api-tier": true, "static-edge": true}
	if len(got) != len(want) {
		t.Fatalf("expected %d distinct groups, got %d: %v", len(want), len(got), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected group %q in TargetGroups()", g)
		}
	}
	if got[0] != "web-tier" {
		t.Errorf("expected the default group to be listed first, got %v", got)
	}
}
