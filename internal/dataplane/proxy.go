package dataplane

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Proxy is an L7 HTTP reverse proxy that, for every incoming request,
// resolves a target backend group via a RouteTable (host/path/method
// rules, falling back to the instance's default group), then selects a
// backend from that group's BackendList. It is deliberately "dumb" — all
// routing intelligence (which rules exist, which backends exist, their
// weights) is pushed to it by the control plane; the proxy itself just
// resolves, picks the next backend, and forwards.
type Proxy struct {
	routes *RouteTable
	groups *GroupManager
	rp     *httputil.ReverseProxy
}

// contextKey avoids collisions with other packages' context values.
type contextKey int

const backendAddrKey contextKey = iota

func contextWithBackendAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, backendAddrKey, addr)
}

// NewProxy creates a reverse proxy that resolves each request's target
// group via routes and selects a backend within that group via groups. A
// single httputil.ReverseProxy is reused across all requests; the target
// backend is selected per-request via the Rewrite hook rather than
// constructing a new ReverseProxy per call.
func NewProxy(routes *RouteTable, groups *GroupManager) *Proxy {
	p := &Proxy{routes: routes, groups: groups}

	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			addr, _ := pr.In.Context().Value(backendAddrKey).(string)
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			addr, _ := r.Context().Value(backendAddrKey).(string)
			log.Printf("dataplane: proxy error forwarding to %s: %v", addr, err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}

	return p
}

// Handler returns an http.Handler that resolves each request's target
// backend group via the route table, then proxies to the next selected
// backend within that group. Returns 503 if no backends are currently
// available for the resolved group.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := p.routes.Resolve(r.Host, r.URL.Path, r.Method)
		backends := p.groups.Ensure(group)

		addr, ok := backends.Next()
		if !ok {
			http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
			return
		}
		// Next() has already incremented this backend's active-connection
		// counter; release it once the proxied request (including
		// streaming the response back to the client) has fully completed,
		// so least_connections reflects real in-flight load rather than
		// only ever growing.
		defer backends.Release(addr)

		ctx := contextWithBackendAddr(r.Context(), addr)
		p.rp.ServeHTTP(w, r.WithContext(ctx))
	})
}
