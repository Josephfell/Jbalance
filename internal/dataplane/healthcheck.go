package dataplane

import (
	"context"
	"log"
	"net"
	"sync"
	"time"
)

// HealthChecker periodically probes every backend currently known to a
// BackendList and marks it healthy/unhealthy based on the outcome. Uses a
// plain TCP connect check — protocol-agnostic and good enough to detect a
// backend that's down or unreachable, which is the common failure mode a
// data plane needs to react to quickly.
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

	mu     sync.Mutex
	streak map[string]int // address -> current consecutive streak; positive = successes, negative = failures
}

// NewHealthChecker creates a health checker with sensible defaults:
// checks every 5s, 2s timeout per check, 3 consecutive failures to mark
// unhealthy, 2 consecutive successes to mark healthy again.
func NewHealthChecker(backends *BackendList) *HealthChecker {
	return &HealthChecker{
		backends:         backends,
		Interval:         5 * time.Second,
		Timeout:          2 * time.Second,
		FailureThreshold: 3,
		SuccessThreshold: 2,
		streak:           make(map[string]int),
	}
}

// Run starts the health check loop. Blocks until ctx is cancelled.
func (h *HealthChecker) Run(ctx context.Context) {
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
	checkCtx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(checkCtx, "tcp", addr)
	if err == nil {
		_ = conn.Close() // best-effort — we only care about connect success/failure here
	}
	h.recordResult(addr, err == nil)
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
	log.Printf("dataplane: backend %s marked %s", addr, state)
}
