package controlplane

import (
	"path/filepath"
	"testing"
)

func TestRoute_Matches_HostExact(t *testing.T) {
	r := Route{Host: "api.acme.io", PathPrefix: "/", TargetGroup: "api-tier"}
	if !r.Matches("api.acme.io", "/v1/orders", "GET") {
		t.Error("expected exact host match to match")
	}
	if r.Matches("acme.io", "/v1/orders", "GET") {
		t.Error("expected a different host not to match")
	}
}

func TestRoute_Matches_HostCaseInsensitive(t *testing.T) {
	r := Route{Host: "API.ACME.IO", PathPrefix: "/", TargetGroup: "api-tier"}
	if !r.Matches("api.acme.io", "/", "GET") {
		t.Error("expected host matching to be case-insensitive")
	}
}

func TestRoute_Matches_WildcardHost(t *testing.T) {
	for _, host := range []string{"*", ""} {
		r := Route{Host: host, PathPrefix: "/", TargetGroup: "web-tier"}
		if !r.Matches("anything.example.com", "/", "GET") {
			t.Errorf("expected host %q to match any host", host)
		}
	}
}

func TestRoute_Matches_PathPrefix(t *testing.T) {
	r := Route{PathPrefix: "/api/", TargetGroup: "api-tier"}
	if !r.Matches("", "/api/v1/orders", "GET") {
		t.Error("expected /api/v1/orders to match prefix /api/")
	}
	if r.Matches("", "/static/logo.png", "GET") {
		t.Error("expected /static/logo.png not to match prefix /api/")
	}
}

func TestRoute_Matches_RootPathMatchesEverything(t *testing.T) {
	for _, prefix := range []string{"/", ""} {
		r := Route{PathPrefix: prefix, TargetGroup: "web-tier"}
		if !r.Matches("", "/anything/at/all", "GET") {
			t.Errorf("expected path prefix %q to match every path", prefix)
		}
	}
}

func TestRoute_Matches_Methods(t *testing.T) {
	r := Route{PathPrefix: "/", Methods: []string{"POST", "PUT"}, TargetGroup: "checkout-tier"}
	if !r.Matches("", "/checkout", "POST") {
		t.Error("expected POST to match")
	}
	if !r.Matches("", "/checkout", "put") {
		t.Error("expected method matching to be case-insensitive")
	}
	if r.Matches("", "/checkout", "GET") {
		t.Error("expected GET not to match a POST/PUT-only rule")
	}
}

func TestRoute_Matches_EmptyMethodsMeansAny(t *testing.T) {
	r := Route{PathPrefix: "/", TargetGroup: "web-tier"}
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if !r.Matches("", "/", m) {
			t.Errorf("expected method %q to match a rule with no method restriction", m)
		}
	}
}

func TestRouteStore_SetAndGet(t *testing.T) {
	s := NewRouteStore("")
	routes := []Route{
		{Host: "acme.io", PathPrefix: "/static/", TargetGroup: "static-edge"},
		{PathPrefix: "/", TargetGroup: "web-tier"},
	}
	if err := s.Set(routes); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	got := s.Routes()
	if len(got) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(got))
	}
	if got[0].TargetGroup != "static-edge" || got[1].TargetGroup != "web-tier" {
		t.Errorf("expected evaluation order to be preserved, got %+v", got)
	}
	if s.Version() != 1 {
		t.Errorf("expected version 1 after the first Set, got %d", s.Version())
	}
}

func TestRouteStore_SetBumpsVersionEachTime(t *testing.T) {
	s := NewRouteStore("")
	_ = s.Set([]Route{{PathPrefix: "/", TargetGroup: "web-tier"}})
	_ = s.Set([]Route{{PathPrefix: "/", TargetGroup: "web-tier"}})
	if s.Version() != 2 {
		t.Errorf("expected version to increment on every Set call, got %d", s.Version())
	}
}

func TestRouteStore_RoutesReturnsDefensiveCopy(t *testing.T) {
	s := NewRouteStore("")
	_ = s.Set([]Route{{PathPrefix: "/", TargetGroup: "web-tier"}})

	got := s.Routes()
	got[0].TargetGroup = "mutated"

	got2 := s.Routes()
	if got2[0].TargetGroup != "web-tier" {
		t.Error("expected Routes() to return a defensive copy, not a reference into internal state")
	}
}

func TestRouteStore_EmptyByDefault(t *testing.T) {
	s := NewRouteStore("")
	if got := s.Routes(); len(got) != 0 {
		t.Errorf("expected an empty route table by default, got %v", got)
	}
	if s.Version() != 0 {
		t.Errorf("expected version 0 before any Set call, got %d", s.Version())
	}
}

func TestRouteStore_PersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")

	s1 := NewRouteStore(path)
	routes := []Route{
		{Host: "acme.io", PathPrefix: "/api/", Methods: []string{"GET", "POST"}, TargetGroup: "api-tier", Name: "api"},
		{PathPrefix: "/", TargetGroup: "web-tier", Name: "default"},
	}
	if err := s1.Set(routes); err != nil {
		t.Fatalf("Set() error: %v", err)
	}

	s2 := NewRouteStore(path)
	got := s2.Routes()
	if len(got) != 2 {
		t.Fatalf("expected 2 reloaded routes, got %d", len(got))
	}
	if got[0].Host != "acme.io" || got[0].Name != "api" || len(got[0].Methods) != 2 {
		t.Errorf("expected reloaded route 0 to match what was set, got %+v", got[0])
	}
	if s2.Version() != s1.Version() {
		t.Errorf("expected reloaded version %d to match %d", s2.Version(), s1.Version())
	}
}

func TestRouteStore_MissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := NewRouteStore(path)
	if got := s.Routes(); len(got) != 0 {
		t.Errorf("expected an empty store when the file doesn't exist, got %v", got)
	}
}

func TestRouteStore_SetReplacesEntireTable(t *testing.T) {
	s := NewRouteStore("")
	_ = s.Set([]Route{
		{PathPrefix: "/a/", TargetGroup: "group-a"},
		{PathPrefix: "/b/", TargetGroup: "group-b"},
	})
	_ = s.Set([]Route{{PathPrefix: "/c/", TargetGroup: "group-c"}})

	got := s.Routes()
	if len(got) != 1 || got[0].TargetGroup != "group-c" {
		t.Errorf("expected Set to fully replace the table, got %+v", got)
	}
}
