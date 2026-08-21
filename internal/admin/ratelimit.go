package admin

import (
	"sync"
	"time"
)

// loginLimiter tracks failed login attempts per client IP in memory (no
// persistence needed — a restart clearing rate-limit state is an
// acceptable tradeoff for a local admin tool with no external database).
// After maxFailures consecutive failures from an IP, further attempts are
// blocked until lockout elapses.
type loginLimiter struct {
	maxFailures int
	lockout     time.Duration
	window      time.Duration

	mu   sync.Mutex
	byIP map[string]*loginAttempts
}

type loginAttempts struct {
	failures    int
	firstFailAt time.Time
	lockedUntil time.Time
}

// newLoginLimiter creates a limiter allowing maxFailures failed attempts
// within window before locking an IP out for lockout.
func newLoginLimiter(maxFailures int, window, lockout time.Duration) *loginLimiter {
	return &loginLimiter{
		maxFailures: maxFailures,
		lockout:     lockout,
		window:      window,
		byIP:        make(map[string]*loginAttempts),
	}
}

// Allowed reports whether a login attempt from ip is currently permitted.
func (l *loginLimiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	a, ok := l.byIP[ip]
	if !ok {
		return true
	}
	return time.Now().After(a.lockedUntil)
}

// RecordFailure registers a failed login attempt from ip, locking it out
// once maxFailures is reached within the configured window.
func (l *loginLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	a, ok := l.byIP[ip]
	if !ok || now.Sub(a.firstFailAt) > l.window {
		a = &loginAttempts{firstFailAt: now}
		l.byIP[ip] = a
	}

	a.failures++
	if a.failures >= l.maxFailures {
		a.lockedUntil = now.Add(l.lockout)
	}
}

// RecordSuccess clears any failure history for ip.
func (l *loginLimiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byIP, ip)
}
