package dataplane

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// HealthCheckMode selects how HealthChecker probes each backend.
type HealthCheckMode string

const (
	// HealthCheckTCP does a plain TCP connect probe: healthy if the
	// connection is accepted. Protocol-agnostic and the historical
	// default.
	HealthCheckTCP HealthCheckMode = "tcp"
	// HealthCheckHTTP issues an HTTP GET to a configured path and treats
	// the backend as healthy only if the response status falls in the
	// expected class. This catches a backend that accepts TCP connections
	// but is actually returning errors (e.g. 500s) — a failure mode a
	// plain connect probe cannot see.
	HealthCheckHTTP HealthCheckMode = "http"
)

// HealthChecker periodically probes every backend currently known to a
// BackendList and marks it healthy/unhealthy based on the outcome.
//
// Two probe modes are supported (Mode):
//   - tcp (default): a plain TCP connect check — protocol-agnostic and
//     good enough to detect a backend that's down or unreachable.
//   - http: an HTTP GET to HTTPPath, healthy only if the response status
//     matches the expected class (HTTPExpectStatus). This detects a
//     backend that is reachable at the TCP layer but serving errors,
//     which a connect probe silently treats as healthy.
//
// A backend must fail consecutively FailureThreshold times before being
// marked unhealthy, and succeed consecutively SuccessThreshold times after
// that before being marked healthy again — this hysteresis avoids flapping
// a backend in and out of rotation over a single transient blip.
type HealthChecker struct {
	backends *BackendList

	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	SuccessThreshold int

	// Mode selects the probe type; empty is treated as tcp.
	Mode HealthCheckMode
	// HTTPPath is the request path for http-mode probes (e.g. "/healthz").
	// Defaults to "/" if empty.
	HTTPPath string
	// HTTPExpectStatus is the exact status code an http-mode probe must
	// receive to count as healthy. 0 means "any 2xx" (the common case,
	// so operators don't have to name a specific code).
	HTTPExpectStatus int
	// HTTPScheme is "http" (default) or "https" for the probe request.
	HTTPScheme string
	// HTTPHost, if set, overrides the Host header sent with the probe
	// (useful when a backend routes by virtual host). Defaults to the
	// backend address.
	HTTPHost string

	httpClient *http.Client

	mu     sync.Mutex
	streak map[string]int // address -> current consecutive streak; positive = successes, negative = failures
}

// NewHealthChecker creates a health checker with sensible defaults:
// tcp-mode, checks every 5s, 2s timeout per check, 3 consecutive failures
// to mark unhealthy, 2 consecutive successes to mark healthy again.
func NewHealthChecker(backends *BackendList) *HealthChecker {
	return &HealthChecker{
		backends:         backends,
		Interval:         5 * time.Second,
		Timeout:          2 * time.Second,
		FailureThreshold: 3,
		SuccessThreshold: 2,
		Mode:             HealthCheckTCP,
		streak:           make(map[string]int),
	}
}

// Run starts the health check loop. Blocks until ctx is cancelled.
func (h *HealthChecker) Run(ctx context.Context) {
	// For http mode, build a client whose per-request timeout matches the
	// configured probe Timeout, and that does not follow redirects (a 3xx
	// from a health endpoint is a signal in its own right, not something
	// to chase). Built once here rather than per-probe.
	if h.mode() == HealthCheckHTTP {
		h.httpClient = &http.Client{
			Timeout: h.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	ticker := time.NewTicker(h.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkAll(ctx)
		}
	}
}

func (h *HealthChecker) mode() HealthCheckMode {
	if h.Mode == HealthCheckHTTP {
		return HealthCheckHTTP
	}
	return HealthCheckTCP
}

func (h *HealthChecker) checkAll(ctx context.Context) {
	addrs := h.backends.Addresses()

	// Probe concurrently — with many backends, sequential probing at
	// Timeout=2s each would make the effective check interval balloon.
	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			h.checkOne(ctx, addr)
		}(addr)
	}
	wg.Wait()
}

func (h *HealthChecker) checkOne(ctx context.Context, addr string) {
	var ok bool
	if h.mode() == HealthCheckHTTP {
		ok = h.probeHTTP(ctx, addr)
	} else {
		ok = h.probeTCP(ctx, addr)
	}
	h.recordResult(addr, ok)
}

// probeTCP reports whether a plain TCP connection to addr succeeds.
func (h *HealthChecker) probeTCP(ctx context.Context, addr string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(checkCtx, "tcp", addr)
	if err == nil {
		_ = conn.Close() // best-effort — we only care about connect success/failure here
	}
	return err == nil
}

// probeHTTP issues a GET to addr's configured health path and reports
// whether the response status matches the expected class. Any transport
// error, timeout, or non-matching status counts as a failure.
func (h *HealthChecker) probeHTTP(ctx context.Context, addr string) bool {
	scheme := h.HTTPScheme
	if scheme == "" {
		scheme = "http"
	}
	path := h.HTTPPath
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}

	url := scheme + "://" + addr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if h.HTTPHost != "" {
		req.Host = h.HTTPHost
	}

	client := h.httpClient
	if client == nil {
		// Defensive: Run builds the client, but a caller invoking
		// checkOne directly (e.g. a test) shouldn't nil-panic.
		client = &http.Client{Timeout: h.Timeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return h.statusHealthy(resp.StatusCode)
}

// statusHealthy reports whether a probe response status counts as healthy:
// an exact match against HTTPExpectStatus when one is configured,
// otherwise any 2xx.
func (h *HealthChecker) statusHealthy(status int) bool {
	if h.HTTPExpectStatus != 0 {
		return status == h.HTTPExpectStatus
	}
	return status >= 200 && status < 300
}

func (h *HealthChecker) recordResult(addr string, success bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	streak := h.streak[addr]
	if success {
		if streak < 0 {
			streak = 0
		}
		streak++
	} else {
		if streak > 0 {
			streak = 0
		}
		streak--
	}
	h.streak[addr] = streak

	if success && streak >= h.SuccessThreshold {
		h.markHealthy(addr, true)
	} else if !success && -streak >= h.FailureThreshold {
		h.markHealthy(addr, false)
	}
}

func (h *HealthChecker) markHealthy(addr string, healthy bool) {
	h.backends.SetHealth(addr, healthy)
	state := "unhealthy"
	if healthy {
		state = "healthy"
	}
	log.Printf("dataplane: backend %s marked %s (%s probe)", addr, state, h.mode())
}
