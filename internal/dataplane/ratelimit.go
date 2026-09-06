// Package dataplane: ratelimit.go implements per-client request rate
// limiting for the L7 (HTTP) proxy. The limit is per backend group and
// pushed from the control plane as part of the group's BackendSet (like
// the load-balancing algorithm and sticky-session config), so it is a
// runtime admin-UI control rather than a startup flag.
//
// Each group has its own limiter; within a group, each client (keyed on
// IP) gets its own token bucket at the configured rate/burst. A client
// that outpaces its bucket gets HTTP 429.
package dataplane

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig is a group's per-client rate-limit setting, mirrored
// from the pb.BackendSet fields (kept as a local struct so BackendList's
// surface doesn't leak the wire type, same as StickyConfig).
type RateLimitConfig struct {
	// Enabled turns rate limiting on for the group.
	Enabled bool
	// RPS is the sustained per-client rate (requests per second).
	RPS float64
	// Burst is the token-bucket depth. When <= 0, the limiter defaults it
	// to RPS (a one-second burst).
	Burst int
}

// groupRateLimiter holds one group's per-client token buckets. It is safe
// for concurrent use. Buckets are created lazily per client IP and
// swept periodically so a long-lived process doesn't accumulate a bucket
// for every IP ever seen.
type groupRateLimiter struct {
	mu       sync.Mutex
	cfg      RateLimitConfig
	clients  map[string]*clientBucket
	lastKeep time.Time
}

type clientBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// idleBucketTTL is how long an unused client bucket is retained before
// the sweep discards it — long enough that a client on a slow cadence
// keeps its bucket, short enough to bound memory.
const idleBucketTTL = 10 * time.Minute

func newGroupRateLimiter() *groupRateLimiter {
	return &groupRateLimiter{clients: make(map[string]*clientBucket), lastKeep: time.Now()}
}

// setConfig updates the limiter's config. When the rate/burst changes,
// existing per-client buckets are dropped so every client picks up the
// new limit on its next request (rather than the old bucket lingering).
func (g *groupRateLimiter) setConfig(cfg RateLimitConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cfg == g.cfg {
		return
	}
	g.cfg = cfg
	g.clients = make(map[string]*clientBucket)
}

// allow reports whether a request from clientKey is permitted right now,
// consuming a token if so. Always allows when rate limiting is disabled
// or misconfigured (rate <= 0), so a bad config can never wedge the
// group closed.
func (g *groupRateLimiter) allow(clientKey string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled || g.cfg.RPS <= 0 {
		return true
	}

	now := time.Now()
	g.sweepLocked(now)

	b, ok := g.clients[clientKey]
	if !ok {
		burst := g.cfg.Burst
		if burst <= 0 {
			burst = int(g.cfg.RPS)
			if burst < 1 {
				burst = 1
			}
		}
		b = &clientBucket{limiter: rate.NewLimiter(rate.Limit(g.cfg.RPS), burst)}
		g.clients[clientKey] = b
	}
	b.lastSeen = now
	return b.limiter.Allow()
}

// sweepLocked discards client buckets not seen within idleBucketTTL.
// Called opportunistically from allow (at most once per minute) so there
// is no separate goroutine to manage. Callers must hold g.mu.
func (g *groupRateLimiter) sweepLocked(now time.Time) {
	if now.Sub(g.lastKeep) < time.Minute {
		return
	}
	g.lastKeep = now
	for k, b := range g.clients {
		if now.Sub(b.lastSeen) > idleBucketTTL {
			delete(g.clients, k)
		}
	}
}
