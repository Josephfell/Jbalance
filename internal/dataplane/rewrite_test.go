package dataplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestResolveRouteReturnsRewrite(t *testing.T) {
	rt := NewRouteTable("default")
	rt.Update(&pb.RouteTable{
		Version: 1,
		Routes: []*pb.Route{
			{
				PathPrefix:  "/api/",
				TargetGroup: "api-tier",
				Rewrite: &pb.RouteRewrite{
					StripPathPrefix:      "/api",
					SetRequestHeaders:    map[string]string{"X-From": "edge"},
					RemoveRequestHeaders: []string{"X-Debug"},
					SetResponseHeaders:   map[string]string{"X-Served-By": "jbalance"},
				},
			},
		},
	})

	group, rw := rt.ResolveRoute("", "/api/v1/orders", "GET")
	if group != "api-tier" {
		t.Fatalf("group = %q, want api-tier", group)
	}
	if rw.isZero() {
		t.Fatal("expected a non-zero rewrite")
	}

	// A non-matching request gets the default group and a zero rewrite.
	g2, rw2 := rt.ResolveRoute("", "/other", "GET")
	if g2 != "default" || !rw2.isZero() {
		t.Fatalf("non-match = (%q, zero=%v), want (default, zero=true)", g2, rw2.isZero())
	}
}

func TestApplyRequestRewrite(t *testing.T) {
	rw := routeRewrite{
		stripPathPrefix:      "/api",
		addPathPrefix:        "/v2",
		setRequestHeaders:    map[string]string{"X-From": "edge"},
		removeRequestHeaders: []string{"X-Debug"},
	}
	r := httptest.NewRequest("GET", "http://example.com/api/orders", nil)
	r.Header.Set("X-Debug", "1")

	rw.applyRequest(r)

	if r.URL.Path != "/v2/orders" {
		t.Errorf("path = %q, want /v2/orders", r.URL.Path)
	}
	if r.Header.Get("X-From") != "edge" {
		t.Errorf("X-From = %q, want edge", r.Header.Get("X-From"))
	}
	if r.Header.Get("X-Debug") != "" {
		t.Errorf("X-Debug should have been removed, got %q", r.Header.Get("X-Debug"))
	}
}

func TestApplyRequestStripLeavesLeadingSlash(t *testing.T) {
	rw := routeRewrite{stripPathPrefix: "/api"}
	// Stripping "/api" from "/api" would leave "" — must become "/".
	r := httptest.NewRequest("GET", "http://example.com/api", nil)
	rw.applyRequest(r)
	if r.URL.Path != "/" {
		t.Errorf("path = %q, want /", r.URL.Path)
	}
}

func TestApplyResponseRewrite(t *testing.T) {
	rw := routeRewrite{
		setResponseHeaders:    map[string]string{"X-Served-By": "jbalance"},
		removeResponseHeaders: []string{"Server"},
	}
	h := http.Header{}
	h.Set("Server", "backend/1.0")
	rw.applyResponse(h)
	if h.Get("X-Served-By") != "jbalance" {
		t.Errorf("X-Served-By = %q, want jbalance", h.Get("X-Served-By"))
	}
	if h.Get("Server") != "" {
		t.Errorf("Server should have been removed, got %q", h.Get("Server"))
	}
}

func TestZeroRewriteIsNoOp(t *testing.T) {
	var rw routeRewrite
	r := httptest.NewRequest("GET", "http://example.com/api/x", nil)
	rw.applyRequest(r)
	if r.URL.Path != "/api/x" {
		t.Errorf("zero rewrite changed path to %q", r.URL.Path)
	}
}
