package dataplane

import (
	"net/http"
	"net/url"
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func hdr(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

func TestRouteTable_HeaderMatch_GatesRouting(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			// A request to /api/ carrying X-Api-Version: 2 goes to api-v2;
			// otherwise it falls through to the general api-tier rule.
			{PathPrefix: "/api/", TargetGroup: "api-v2", HeaderMatches: []*pb.HeaderMatch{{Name: "X-Api-Version", Value: "2"}}},
			{PathPrefix: "/api/", TargetGroup: "api-tier"},
		},
	})

	if got := rt.Resolve("", "/api/orders", "GET", hdr("X-Api-Version", "2"), nil); got != "api-v2" {
		t.Errorf("expected header-gated rule to win, got %q", got)
	}
	if got := rt.Resolve("", "/api/orders", "GET", hdr("X-Api-Version", "1"), nil); got != "api-tier" {
		t.Errorf("expected a non-matching header value to fall through, got %q", got)
	}
	if got := rt.Resolve("", "/api/orders", "GET", nil, nil); got != "api-tier" {
		t.Errorf("expected a missing header to fall through, got %q", got)
	}
}

func TestRouteTable_HeaderPresenceMatch(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/", TargetGroup: "debug-tier", HeaderMatches: []*pb.HeaderMatch{{Name: "X-Debug"}}},
		},
	})

	if got := rt.Resolve("", "/", "GET", hdr("X-Debug", "1"), nil); got != "debug-tier" {
		t.Errorf("expected presence-only header to match, got %q", got)
	}
	if got := rt.Resolve("", "/", "GET", nil, nil); got != "web-tier" {
		t.Errorf("expected a request without the header to fall back to default, got %q", got)
	}
}

func TestRouteTable_QueryMatch_GatesRouting(t *testing.T) {
	rt := NewRouteTable("stable")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/", TargetGroup: "canary", QueryMatches: []*pb.QueryMatch{{Name: "canary", Value: "true"}}},
			{PathPrefix: "/", TargetGroup: "stable"},
		},
	})

	if got := rt.Resolve("", "/x", "GET", nil, url.Values{"canary": {"true"}}); got != "canary" {
		t.Errorf("expected ?canary=true to route to canary, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", nil, url.Values{"canary": {"false"}}); got != "stable" {
		t.Errorf("expected ?canary=false to fall through to stable, got %q", got)
	}
	if got := rt.Resolve("", "/x", "GET", nil, nil); got != "stable" {
		t.Errorf("expected no query param to fall through to stable, got %q", got)
	}
}

func TestRouteTable_HeaderAndQueryAnded(t *testing.T) {
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:    "/api/",
				TargetGroup:   "special",
				HeaderMatches: []*pb.HeaderMatch{{Name: "X-Api-Version", Value: "2"}},
				QueryMatches:  []*pb.QueryMatch{{Name: "canary", Value: "true"}},
			},
			{PathPrefix: "/api/", TargetGroup: "api-tier"},
		},
	})

	both := func(h http.Header, q url.Values) string { return rt.Resolve("", "/api/x", "GET", h, q) }

	if got := both(hdr("X-Api-Version", "2"), url.Values{"canary": {"true"}}); got != "special" {
		t.Errorf("expected both conditions satisfied to match special, got %q", got)
	}
	if got := both(hdr("X-Api-Version", "2"), nil); got != "api-tier" {
		t.Errorf("expected header-only (no query) to fall through, got %q", got)
	}
	if got := both(nil, url.Values{"canary": {"true"}}); got != "api-tier" {
		t.Errorf("expected query-only (no header) to fall through, got %q", got)
	}
}

func TestRouteTable_MatchConditions_SurviveProtoRoundTrip(t *testing.T) {
	// An empty-name matcher pushed over the wire must be dropped, not
	// treated as an always-fail condition.
	rt := NewRouteTable("web-tier")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{PathPrefix: "/", TargetGroup: "g", HeaderMatches: []*pb.HeaderMatch{{Name: ""}}, QueryMatches: []*pb.QueryMatch{{Name: ""}}},
		},
	})
	if got := rt.Resolve("", "/x", "GET", nil, nil); got != "g" {
		t.Errorf("expected empty-name matchers to be ignored (rule still matches), got %q", got)
	}
}
