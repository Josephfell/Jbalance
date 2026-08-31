package dataplane

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Proxy is an L7 HTTP reverse proxy that, for every incoming request,
// resolves a target backend group via a RouteTable (host/path/method
// rules, falling back to the instance's default group), then selects a
// backend from that group's BackendList. It is deliberately "dumb" — all
// routing intelligence (which rules exist, which backends exist, their
// weights) is pushed to it by the control plane; the proxy itself just
// resolves, picks the next backend, and forwards.
type Proxy struct {
	routes  *RouteTable
	groups  *GroupManager
	rp      *httputil.ReverseProxy
	metrics *Metrics
}

// contextKey avoids collisions with other packages' context values.
type contextKey int

const backendAddrKey contextKey = iota

func contextWithBackendAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, backendAddrKey, addr)
}

// statusCodeKey stores the eventual response status code in the request
// context, set by rp.ModifyResponse (for a successful proxied response)
// or the ErrorHandler (for a failed one) — read back by Handler after
// ServeHTTP returns so a single metrics observation covers both outcomes
// rather than needing separate recording paths.
const statusCodeKey contextKey = iota + 1

// NewProxy creates a reverse proxy that resolves each request's target
// group via routes and selects a backend within that group via groups.
// metrics may be nil, in which case no request metrics are recorded (a
// nil registry is used by tests that don't care about metrics). A single
// httputil.ReverseProxy is reused across all requests; the target backend
// is selected per-request via the Rewrite hook rather than constructing a
// new ReverseProxy per call.
func NewProxy(routes *RouteTable, groups *GroupManager, metrics *Metrics) *Proxy {
	p := &Proxy{routes: routes, groups: groups, metrics: metrics}

	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			addr, _ := pr.In.Context().Value(backendAddrKey).(string)
			pr.SetURL(&url.URL{Scheme: "http", Host: addr})
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if ptr, ok := resp.Request.Context().Value(statusCodeKey).(*int); ok {
				*ptr = resp.StatusCode
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			addr, _ := r.Context().Value(backendAddrKey).(string)
			log.Printf("dataplane: proxy error forwarding to %s: %v", addr, err)
			if ptr, ok := r.Context().Value(statusCodeKey).(*int); ok {
				*ptr = http.StatusBadGateway
			}
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}

	return p
}

// Handler returns an http.Handler that resolves each request's target
// backend group via the route table, then proxies to a backend within
// that group — honouring cookie-based session affinity if the group has
// it enabled, otherwise using the group's normal load-balancing
// algorithm. Returns 503 if no backends are currently available for the
// resolved group.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := p.routes.Resolve(r.Host, r.URL.Path, r.Method)
		backends := p.groups.Ensure(group)

		addr, ok := selectBackend(w, r, backends)
		if !ok {
			http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
			if p.metrics != nil {
				p.metrics.ObserveHTTPRequest(group, http.StatusServiceUnavailable, 0)
			}
			return
		}
		// Either path through selectBackend has already incremented this
		// backend's active-connection counter (Next() directly, or PinTo()
		// on the sticky path); release it once the proxied request
		// (including streaming the response back to the client) has fully
		// completed, so least_connections reflects real in-flight load
		// rather than only ever growing.
		defer backends.Release(addr)

		start := time.Now()
		if p.metrics != nil {
			p.metrics.SetActiveConnections(group, 1)
			defer p.metrics.SetActiveConnections(group, -1)
		}

		statusCode := http.StatusOK // default if neither ModifyResponse nor ErrorHandler overwrite it (shouldn't happen, but avoids an unset value)
		ctx := contextWithBackendAddr(r.Context(), addr)
		ctx = context.WithValue(ctx, statusCodeKey, &statusCode)
		p.rp.ServeHTTP(w, r.WithContext(ctx))

		if p.metrics != nil {
			p.metrics.ObserveHTTPRequest(group, statusCode, time.Since(start))
		}
	})
}

// selectBackend picks the backend to proxy this request to: if the group
// has sticky sessions enabled and the request carries a valid,
// still-healthy affinity cookie, that pinned backend is reused (refreshing
// the cookie's TTL); otherwise a new backend is chosen via the group's
// normal algorithm and, if sticky sessions are enabled, pinned by setting
// the affinity cookie on the response for subsequent requests to reuse.
//
// The cookie's value is the backend's own address (e.g. "10.2.1.14:8080")
// rather than an opaque token — deliberately simple, at the cost of
// revealing an internal backend address to the client that holds the
// cookie. This is a real (if minor) information disclosure tradeoff:
// resolving it properly would mean each data plane instance maintaining a
// server-side token->address mapping, which is state this proxy
// otherwise has none of (every data plane instance can restart
// independently with zero session loss beyond backend health/counters).
// Tampering isn't a meaningful risk either way: PinTo validates the
// cookie's value against the group's actual healthy backend list before
// honouring it, so a forged value can only ever resolve to a backend
// that's already a legitimate member of the group, never an arbitrary
// address.
func selectBackend(w http.ResponseWriter, r *http.Request, backends *BackendList) (string, bool) {
	sticky := backends.Sticky()
	if !sticky.Enabled {
		return backends.Next()
	}

	cookieName := sticky.CookieName
	if cookieName == "" {
		cookieName = "jb_affinity"
	}

	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		if backends.PinTo(cookie.Value) {
			setAffinityCookie(w, cookieName, cookie.Value, sticky.TTL)
			return cookie.Value, true
		}
		// The pinned backend is gone or unhealthy — fall through to
		// normal selection and pin the client to whatever's chosen next,
		// rather than failing the request outright.
	}

	addr, ok := backends.Next()
	if !ok {
		return "", false
	}
	setAffinityCookie(w, cookieName, addr, sticky.TTL)
	return addr, true
}

// setAffinityCookie sets (or refreshes) the sticky-session cookie
// pinning the client to addr. HttpOnly since this cookie carries a
// backend address, not something client-side script has any legitimate
// reason to read; SameSite=Lax rather than Strict since a cross-site
// navigation into a proxied application is a normal, unremarkable case
// that affinity shouldn't break.
func setAffinityCookie(w http.ResponseWriter, cookieName, addr string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    addr,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
