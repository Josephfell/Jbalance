package dataplane

import (
	"net/http"
	"net/url"
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

// header builds an http.Header from name/value pairs for concise test setup.
func header(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

// query builds url.Values from name/value pairs.
func query(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Add(pairs[i], pairs[i+1])
	}
	return q
}

// TestRouteTable_HeaderMatchGatesRoute verifies that a route carrying a
// header_matches condition is selected only when the request header is
// present with the required value, and otherwise falls through.
func TestRouteTable_HeaderMatchGatesRoute(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:    "/api/",
				TargetGroup:   "api-v2-tier",
				HeaderMatches: []*pb.HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
			},
			{PathPrefix: "/api/", TargetGroup: "api-tier"},
		},
	})

	// Match: header present with the exact value -> gated rule wins.
	if got := rt.Resolve("", "/api/orders", "GET", header("X-Api-Version", "2"), nil); got != "api-v2-tier" {
		t.Errorf("expected matching header to select the gated rule, got %q", got)
	}
	// Miss: header present but wrong value -> falls through to the general rule.
	if got := rt.Resolve("", "/api/orders", "GET", header("X-Api-Version", "1"), nil); got != "api-tier" {
		t.Errorf("expected a wrong header value to fall through, got %q", got)
	}
	// Miss: header absent entirely -> falls through.
	if got := rt.Resolve("", "/api/orders", "GET", nil, nil); got != "api-tier" {
		t.Errorf("expected an absent header to fall through, got %q", got)
	}
}

// TestRouteTable_HeaderPresenceOnlyMatch verifies that an empty match value
// matches on mere presence of the header, regardless of its value.
func TestRouteTable_HeaderPresenceOnlyMatch(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:    "/",
				TargetGroup:   "debug-tier",
				HeaderMatches: []*pb.HeaderMatch{{Name: "X-Debug"}}, // presence only
			},
		},
	})

	if got := rt.Resolve("", "/x", "GET", header("X-Debug", "anything"), nil); got != "debug-tier" {
		t.Errorf("expected presence-only header match to select the rule, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", header("X-Debug", ""), nil); got != "debug-tier" {
		t.Errorf("expected presence-only match to accept an empty value, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", nil, nil); got != "web-tier" {
		t.Errorf("expected an absent header to fall back to the default group, got %q", got)
	}
}

// TestRouteTable_QueryMatchGatesRoute verifies query-parameter gating for
// both hit and miss cases.
func TestRouteTable_QueryMatchGatesRoute(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:   "/",
				TargetGroup:  "canary-tier",
				QueryMatches: []*pb.QueryMatch{{Name: "canary", Value: "true"}},
			},
			{PathPrefix: "/", TargetGroup: "stable-tier"},
		},
	})

	if got := rt.Resolve("", "/home", "GET", nil, query("canary", "true")); got != "canary-tier" {
		t.Errorf("expected matching query param to select the canary rule, got %q", got)
	}
	if got := rt.Resolve("", "/home", "GET", nil, query("canary", "false")); got != "stable-tier" {
		t.Errorf("expected a wrong query value to fall through, got %q", got)
	}
	if got := rt.Resolve("", "/home", "GET", nil, nil); got != "stable-tier" {
		t.Errorf("expected an absent query param to fall through, got %q", got)
	}
}

// TestRouteTable_QueryPresenceOnlyMatch verifies that an empty match value
// matches when the query parameter is present with any value (including
// the bare `?flag` form that yields an empty string value).
func TestRouteTable_QueryPresenceOnlyMatch(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:   "/",
				TargetGroup:  "beta-tier",
				QueryMatches: []*pb.QueryMatch{{Name: "beta"}}, // presence only
			},
		},
	})

	if got := rt.Resolve("", "/x", "GET", nil, query("beta", "1")); got != "beta-tier" {
		t.Errorf("expected presence-only query match to select the rule, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", nil, query("beta", "")); got != "beta-tier" {
		t.Errorf("expected presence-only query match to accept a bare param, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", nil, query("other", "1")); got != "web-tier" {
		t.Errorf("expected an absent query param to fall back to the default group, got %q", got)
	}
}

// TestRouteTable_MultipleConditionsAreANDed verifies that when a route
// declares several header/query conditions, ALL of them must hold for the
// route to match.
func TestRouteTable_MultipleConditionsAreANDed(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:  "/",
				TargetGroup: "gated-tier",
				HeaderMatches: []*pb.HeaderMatch{
					{Name: "X-Api-Version", Value: "2"},
					{Name: "X-Region", Value: "eu"},
				},
				QueryMatches: []*pb.QueryMatch{{Name: "canary", Value: "true"}},
			},
			{PathPrefix: "/", TargetGroup: "fallback-tier"},
		},
	})

	// All three conditions satisfied -> gated rule.
	if got := rt.Resolve("", "/x", "GET",
		header("X-Api-Version", "2", "X-Region", "eu"),
		query("canary", "true")); got != "gated-tier" {
		t.Errorf("expected all conditions met to select the gated rule, got %q", got)
	}
	// One header condition missing -> falls through.
	if got := rt.Resolve("", "/x", "GET",
		header("X-Api-Version", "2"),
		query("canary", "true")); got != "fallback-tier" {
		t.Errorf("expected a missing header condition to fall through, got %q", got)
	}
	// Query condition missing -> falls through.
	if got := rt.Resolve("", "/x", "GET",
		header("X-Api-Version", "2", "X-Region", "eu"),
		nil); got != "fallback-tier" {
		t.Errorf("expected a missing query condition to fall through, got %q", got)
	}
}

// TestRouteTable_HeaderQueryCombinedWithHostPathMethod verifies the extra
// conditions AND with the existing host/path/method matching rather than
// replacing it.
func TestRouteTable_HeaderQueryCombinedWithHostPathMethod(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				Host:          "api.acme.io",
				PathPrefix:    "/v2/",
				Methods:       []string{"POST"},
				TargetGroup:   "api-write-v2",
				HeaderMatches: []*pb.HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
			},
			{Host: "api.acme.io", PathPrefix: "/", TargetGroup: "api-tier"},
		},
	})

	// Everything lines up.
	if got := rt.Resolve("api.acme.io", "/v2/orders", "POST", header("X-Api-Version", "2"), nil); got != "api-write-v2" {
		t.Errorf("expected full match to select api-write-v2, got %q", got)
	}
	// Header matches but method does not -> falls through.
	if got := rt.Resolve("api.acme.io", "/v2/orders", "GET", header("X-Api-Version", "2"), nil); got != "api-tier" {
		t.Errorf("expected a non-matching method to fall through even with the header set, got %q", got)
	}
	// Host/path/method match but header does not -> falls through.
	if got := rt.Resolve("api.acme.io", "/v2/orders", "POST", nil, nil); got != "api-tier" {
		t.Errorf("expected a missing header to fall through even with host/path/method matched, got %q", got)
	}
}

// TestHeaderConditionMet exercises the header matcher directly, including
// the nil-header edge case and case-insensitive header-name lookup.
func TestHeaderConditionMet(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		hm     kvMatch
		want   bool
	}{
		{"nil header, value required", nil, kvMatch{name: "X-A", value: "1"}, false},
		{"nil header, presence only", nil, kvMatch{name: "X-A"}, false},
		{"present exact value", header("X-A", "1"), kvMatch{name: "X-A", value: "1"}, true},
		{"present wrong value", header("X-A", "2"), kvMatch{name: "X-A", value: "1"}, false},
		{"absent, value required", header("X-B", "1"), kvMatch{name: "X-A", value: "1"}, false},
		{"presence only, present", header("X-A", "anything"), kvMatch{name: "X-A"}, true},
		{"presence only, absent", header("X-B", "1"), kvMatch{name: "X-A"}, false},
		{"case-insensitive name lookup", header("x-api-version", "2"), kvMatch{name: "X-Api-Version", value: "2"}, true},
		{"multi-value, one matches", header("X-A", "1", "X-A", "2"), kvMatch{name: "X-A", value: "2"}, true},
		{"value comparison is case-sensitive", header("X-A", "Prod"), kvMatch{name: "X-A", value: "prod"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerConditionMet(tc.header, tc.hm); got != tc.want {
				t.Errorf("headerConditionMet(%v, %+v) = %v, want %v", tc.header, tc.hm, got, tc.want)
			}
		})
	}
}

// TestQueryConditionMet exercises the query matcher directly, including the
// nil-query edge case and the bare-parameter (empty value) case.
func TestQueryConditionMet(t *testing.T) {
	tests := []struct {
		name  string
		query url.Values
		qm    kvMatch
		want  bool
	}{
		{"nil query, value required", nil, kvMatch{name: "a", value: "1"}, false},
		{"nil query, presence only", nil, kvMatch{name: "a"}, false},
		{"present exact value", query("a", "1"), kvMatch{name: "a", value: "1"}, true},
		{"present wrong value", query("a", "2"), kvMatch{name: "a", value: "1"}, false},
		{"absent, value required", query("b", "1"), kvMatch{name: "a", value: "1"}, false},
		{"presence only, present with value", query("a", "1"), kvMatch{name: "a"}, true},
		{"presence only, bare param (empty value)", query("a", ""), kvMatch{name: "a"}, true},
		{"presence only, absent", query("b", "1"), kvMatch{name: "a"}, false},
		{"multi-value, one matches", query("a", "1", "a", "2"), kvMatch{name: "a", value: "2"}, true},
		{"name comparison is case-sensitive", query("A", "1"), kvMatch{name: "a", value: "1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := queryConditionMet(tc.query, tc.qm); got != tc.want {
				t.Errorf("queryConditionMet(%v, %+v) = %v, want %v", tc.query, tc.qm, got, tc.want)
			}
		})
	}
}

// TestKvMatchesFromProto verifies proto->local conversion for both header
// and query matches, including that empty-name entries are skipped.
func TestKvMatchesFromProto(t *testing.T) {
	hms := kvMatchesFromProto([]*pb.HeaderMatch{
		{Name: "X-A", Value: "1"},
		{Name: "", Value: "skip-me"}, // empty name should be dropped
		{Name: "X-B"},
	})
	if len(hms) != 2 {
		t.Fatalf("expected 2 header matches (empty-name dropped), got %d: %+v", len(hms), hms)
	}
	if hms[0] != (kvMatch{name: "X-A", value: "1"}) || hms[1] != (kvMatch{name: "X-B"}) {
		t.Errorf("unexpected header match conversion: %+v", hms)
	}

	qms := queryMatchesFromProto([]*pb.QueryMatch{
		{Name: "canary", Value: "true"},
		{Name: ""}, // dropped
	})
	if len(qms) != 1 {
		t.Fatalf("expected 1 query match (empty-name dropped), got %d: %+v", len(qms), qms)
	}
	if qms[0] != (kvMatch{name: "canary", value: "true"}) {
		t.Errorf("unexpected query match conversion: %+v", qms)
	}

	if kvMatchesFromProto(nil) != nil {
		t.Errorf("expected nil input to yield nil")
	}
	if queryMatchesFromProto(nil) != nil {
		t.Errorf("expected nil input to yield nil")
	}
}
