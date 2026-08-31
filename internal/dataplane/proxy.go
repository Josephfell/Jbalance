package dataplane

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// ProxyConfig bounds how long the L7 proxy will spend talking to a
// backend and how hard it retries a failed attempt. Any zero field falls
// back to the default applied in NewProxy — a zero-value ProxyConfig is
// therefore a valid "sensible defaults" configuration.
type ProxyConfig struct {
	// ConnectTimeout bounds establishing the TCP connection to a backend.
	ConnectTimeout time.Duration
	// ResponseTimeout bounds waiting for the backend's response headers
	// after the request is written (0 disables the limit).
	ResponseTimeout time.Duration
	// MaxRetries is the number of ADDITIONAL backends to try after the
	// first attempt fails at the connection level. 0 means no retries
	// (single attempt). Retries only ever happen for requests with no
	// body to replay (e.g. GET/HEAD), and only before any response byte
	// has been written to the client.
	MaxRetries int
	// RetryBackoff is the base delay between retry attempts; each
	// successive retry waits one more multiple of it (linear backoff).
	RetryBackoff time.Duration
}

const (
	defaultConnectTimeout  = 5 * time.Second
	defaultResponseTimeout = 30 * time.Second
	defaultRetryBackoff    = 50 * time.Millisecond
)

func (c ProxyConfig) withDefaults() ProxyConfig {
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = defaultConnectTimeout
	}
	if c.ResponseTimeout < 0 {
		c.ResponseTimeout = 0
	} else if c.ResponseTimeout == 0 {
		c.ResponseTimeout = defaultResponseTimeout
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = defaultRetryBackoff
	}
	return c
}

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
	cfg     ProxyConfig
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

// upstreamErrKey stores a *bool in the request context that the
// ErrorHandler sets to true when the backend attempt failed at the
// connection level (before response headers arrived). Handler reads it
// back to decide whether a retry against a different backend is warranted.
const upstreamErrKey contextKey = iota + 2

// NewProxy creates a reverse proxy that resolves each request's target
// group via routes and selects a backend within that group via groups.
// metrics may be nil, in which case no request metrics are recorded (a
// nil registry is used by tests that don't care about metrics). A single
// httputil.ReverseProxy is reused across all requests; the target backend
// is selected per-request via the Rewrite hook rather than constructing a
// new ReverseProxy per call.
//
// cfg bounds the per-attempt connect/response timeouts and the bounded
// retry policy; a zero-value ProxyConfig applies sensible defaults.
func NewProxy(routes *RouteTable, groups *GroupManager, metrics *Metrics, cfg ProxyConfig) *Proxy {
	cfg = cfg.withDefaults()
	p := &Proxy{routes: routes, groups: groups, metrics: metrics, cfg: cfg}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseTimeout,
	}

	p.rp = &httputil.ReverseProxy{
		Transport: transport,
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
			if ptr, ok := r.Context().Value(upstreamErrKey).(*bool); ok {
				*ptr = true
			}
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
//
// A connection-level failure against the selected backend is retried
// against a freshly-selected backend, up to cfg.MaxRetries additional
// attempts, but only for requests that carry no body to replay (so the
// upstream request is safely repeatable) and only before any response
// byte has reached the client — a buffered retry writer holds the failed
// attempt's response back so a retry can still succeed cleanly.
func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group := p.routes.Resolve(r.Host, r.URL.Path, r.Method)
		backends := p.groups.Ensure(group)

		start := time.Now()
		if p.metrics != nil {
			p.metrics.SetActiveConnections(group, 1)
			defer p.metrics.SetActiveConnections(group, -1)
		}

		statusCode, ok := p.serveWithRetries(w, r, group, backends)
		if !ok {
			http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
			statusCode = http.StatusServiceUnavailable
		}

		if p.metrics != nil {
			p.metrics.ObserveHTTPRequest(group, statusCode, time.Since(start))
		}
	})
}

// serveWithRetries selects a backend and proxies the request, retrying a
// connection-level failure against a different backend when the request
// is safely retryable. It returns the final status code and whether a
// backend was available at all (false => caller should emit 503).
func (p *Proxy) serveWithRetries(w http.ResponseWriter, r *http.Request, group string, backends *BackendList) (int, bool) {
	retryable := requestRetryable(r)
	attempts := 1
	if retryable {
		attempts += p.cfg.MaxRetries
	}

	for attempt := 0; attempt < attempts; attempt++ {
		addr, ok := selectBackend(w, r, backends)
		if !ok {
			return 0, false
		}

		lastAttempt := attempt == attempts-1
		statusCode, upstreamErr := p.serveOnce(w, r, addr, backends, retryable && !lastAttempt)

		if !upstreamErr || lastAttempt {
			// Either the attempt reached the backend (success or a real
			// upstream status) or we've exhausted retries — done.
			return statusCode, true
		}

		// Connection-level failure and we still have retries left: pick a
		// different backend after a short backoff.
		log.Printf("dataplane: retrying request to group %q after backend %s failed (attempt %d/%d)", group, addr, attempt+1, attempts)
		if p.cfg.RetryBackoff > 0 {
			select {
			case <-r.Context().Done():
				return http.StatusBadGateway, true
			case <-time.After(time.Duration(attempt+1) * p.cfg.RetryBackoff):
			}
		}
	}

	return http.StatusBadGateway, true
}

// serveOnce proxies the request to exactly one backend. When buffered is
// true, the response is written to an in-memory retry writer that holds
// back the output until the attempt is known to have succeeded — so a
// connection-level failure can be retried without the client having seen
// a partial/failed response. When buffered is false, the response is
// streamed straight to the client (the normal, final-attempt path).
//
// Returns the status code and whether the attempt failed at the
// connection level (upstreamErr), which the caller uses to decide whether
// to retry.
func (p *Proxy) serveOnce(w http.ResponseWriter, r *http.Request, addr string, backends *BackendList, buffered bool) (statusCode int, upstreamErr bool) {
	// Either path through selectBackend has already incremented this
	// backend's active-connection counter; release it once this attempt
	// completes so least_connections reflects real in-flight load.
	defer backends.Release(addr)

	statusCode = http.StatusOK
	failed := false
	ctx := contextWithBackendAddr(r.Context(), addr)
	ctx = context.WithValue(ctx, statusCodeKey, &statusCode)
	ctx = context.WithValue(ctx, upstreamErrKey, &failed)
	req := r.WithContext(ctx)

	if buffered {
		rw := newRetryResponseWriter()
		p.rp.ServeHTTP(rw, req)
		if failed {
			// Discard the buffered error response; caller will retry.
			return statusCode, true
		}
		rw.flushTo(w)
		return statusCode, false
	}

	p.rp.ServeHTTP(w, req)
	return statusCode, failed
}

// requestRetryable reports whether a request can be safely re-sent to a
// different backend: it must carry no body (so there is nothing to
// replay) and use an idempotent-by-convention method. This deliberately
// excludes any request with a body, even a GET with one, since the body
// stream is already consumed after the first attempt.
func requestRetryable(r *http.Request) bool {
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
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
