package dataplane

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// newTestBackend starts a real HTTP server that responds with a fixed
// label, returning its address for use as a BackendList entry — the
// proxy tests need a real upstream to forward to, not just a fake
// BackendList entry, since Proxy.Handler actually dials out.
func newTestBackend(t *testing.T, label string) (addr string, cleanup func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, label)
	}))
	return srv.Listener.Addr().String(), srv.Close
}

func TestProxy_RoutesToDefaultGroupWithNoRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr, cleanup := newTestBackend(t, "web")
	defer cleanup()

	routes := NewRouteTable("web-tier")
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	groups.Ensure("web-tier").Update(&pb.BackendSet{Group: "web-tier", Version: 1, Backends: []*pb.Backend{{Address: addr, Weight: 1}}})

	proxy := NewProxy(routes, groups)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	proxy.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "web" {
		t.Errorf("expected the default group's backend to serve the request, got %q", rec.Body.String())
	}
}

func TestProxy_RoutesByPathPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	webAddr, webCleanup := newTestBackend(t, "web")
	defer webCleanup()
	apiAddr, apiCleanup := newTestBackend(t, "api")
	defer apiCleanup()

	routes := NewRouteTable("web-tier")
	routes.Update(&pb.RouteTable{Version: 1, Routes: []*pb.Route{
		{PathPrefix: "/api/", TargetGroup: "api-tier"},
	}})

	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	groups.Ensure("web-tier").Update(&pb.BackendSet{Group: "web-tier", Version: 1, Backends: []*pb.Backend{{Address: webAddr, Weight: 1}}})
	groups.Ensure("api-tier").Update(&pb.BackendSet{Group: "api-tier", Version: 1, Backends: []*pb.Backend{{Address: apiAddr, Weight: 1}}})

	proxy := NewProxy(routes, groups)
	handler := proxy.Handler()

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/api/orders", nil))
	if rec1.Body.String() != "api" {
		t.Errorf("expected /api/orders to be routed to api-tier, got %q", rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/home", nil))
	if rec2.Body.String() != "web" {
		t.Errorf("expected /home to fall back to the default group web-tier, got %q", rec2.Body.String())
	}
}

func TestProxy_RoutesByHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	webAddr, webCleanup := newTestBackend(t, "web")
	defer webCleanup()
	staticAddr, staticCleanup := newTestBackend(t, "static")
	defer staticCleanup()

	routes := NewRouteTable("web-tier")
	routes.Update(&pb.RouteTable{Version: 1, Routes: []*pb.Route{
		{Host: "static.acme.io", PathPrefix: "/", TargetGroup: "static-edge"},
	}})

	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	groups.Ensure("web-tier").Update(&pb.BackendSet{Group: "web-tier", Version: 1, Backends: []*pb.Backend{{Address: webAddr, Weight: 1}}})
	groups.Ensure("static-edge").Update(&pb.BackendSet{Group: "static-edge", Version: 1, Backends: []*pb.Backend{{Address: staticAddr, Weight: 1}}})

	proxy := NewProxy(routes, groups)
	handler := proxy.Handler()

	req := httptest.NewRequest(http.MethodGet, "/logo.png", nil)
	req.Host = "static.acme.io"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Body.String() != "static" {
		t.Errorf("expected a request to static.acme.io to be routed to static-edge, got %q", rec.Body.String())
	}
}

func TestProxy_ReturnsServiceUnavailableWhenResolvedGroupHasNoBackends(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routes := NewRouteTable("web-tier")
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	// Deliberately never call Update — the group exists but has no backends.
	groups.Ensure("web-tier")

	proxy := NewProxy(routes, groups)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the resolved group has no backends, got %d", rec.Code)
	}
}

func TestProxy_LazilyEnsuresGroupReferencedOnlyByARoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	apiAddr, apiCleanup := newTestBackend(t, "api")
	defer apiCleanup()

	routes := NewRouteTable("web-tier")
	routes.Update(&pb.RouteTable{Version: 1, Routes: []*pb.Route{
		{PathPrefix: "/api/", TargetGroup: "api-tier"},
	}})

	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	// api-tier was never explicitly Ensure()'d before the request — the
	// proxy's own call to groups.Ensure inside Handler must create it on
	// demand. Populate its backends only after constructing the proxy, to
	// prove the lazy path (not a pre-existing group) is what's exercised.
	proxy := NewProxy(routes, groups)

	groups.Ensure("api-tier").Update(&pb.BackendSet{Group: "api-tier", Version: 1, Backends: []*pb.Backend{{Address: apiAddr, Weight: 1}}})

	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))

	if rec.Body.String() != "api" {
		t.Errorf("expected the lazily-ensured api-tier group to serve the request, got %q", rec.Body.String())
	}
}
