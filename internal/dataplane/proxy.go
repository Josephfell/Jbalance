package dataplane

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Proxy is an L7 HTTP reverse proxy that selects a backend from a
// BackendList (weighted round-robin) for every incoming request. It is
// deliberately "dumb" — all routing intelligence (which backends exist,
// their weights) is pushed to it by the control plane; the proxy itself
// just picks the next one and forwards.
type Proxy struct {
	backends *BackendList
	rp       *httputil.ReverseProxy
}

// contextKey avoids collisions with other packages' context values.
type contextKey int

const backendAddrKey contextKey = iota

func contextWithBackendAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, backendAddrKey, addr)
}

// NewProxy creates a reverse proxy backed by the given BackendList. A
// single httputil.ReverseProxy is reused across all requests; the target
// backend is selected per-request via the Rewrite hook rather than
// constructing a new ReverseProxy per call.
func NewProxy(backends *BackendList) *Proxy {
	p := &Proxy{backends: backends}

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

// Handler returns an http.Handler that proxies every request to the next
// selected backend. Returns 503 if no backends are currently available.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, ok := p.backends.Next()
		if !ok {
			http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
			return
		}
		// Next() has already incremented this backend's active-connection
		// counter; release it once the proxied request (including
		// streaming the response back to the client) has fully completed,
		// so least_connections reflects real in-flight load rather than
		// only ever growing.
		defer p.backends.Release(addr)

		ctx := contextWithBackendAddr(r.Context(), addr)
		p.rp.ServeHTTP(w, r.WithContext(ctx))
	})
}
