// Package dataplane: recovery.go adds two production-hardening middlewares
// to the L7 HTTP handler chain, both deliberately independent of the proxy
// itself so they wrap it (and each other) as plain http.Handler decorators:
//
//   - RecoveryMiddleware: catches a panic anywhere in the wrapped handler
//     (the proxy, a future middleware, or the stdlib), logs it with the
//     request's request-id and a stack trace, records a metric, and turns
//     it into a 500 — so a single malformed request or a latent bug in one
//     code path can never take down the whole process (and every other
//     in-flight request with it).
//
//   - MaxConnsMiddleware: bounds how many requests are served concurrently.
//     A buffered-channel semaphore admits up to N at once; a request
//     arriving while the limit is saturated is rejected immediately with
//     503 rather than being queued unboundedly, which is what protects the
//     process from memory exhaustion under a connection flood (each queued
//     request otherwise holds a goroutine, its buffers, and its backend
//     connection).
//
//   - MaxBodyMiddleware: caps how many bytes each request body may carry,
//     so an oversized upload is stopped at the proxy before it is streamed
//     to (and buffered by) a backend.
//
// Header-size and header-read-timeout limits (http.Server.MaxHeaderBytes,
// ReadHeaderTimeout) are set on the server itself in cmd/dataplane, since
// they act at the server layer rather than as handler decorators.
package dataplane

import (
	"net/http"
	"runtime"

	"github.com/Josephfell/Jbalance/internal/logging"
)

// RecoveryMiddleware wraps next so that a panic in the wrapped handler is
// recovered, logged (with the request's request-id via the context logger,
// plus method, path and a stack trace), counted in metrics, and answered
// with a 500 — instead of unwinding the goroutine and crashing the whole
// process. group is used only for the panic metric label; metrics may be
// nil (no metric recorded then).
//
// http.ErrAbortHandler is re-panicked, not swallowed: the stdlib uses it
// as a sentinel to abort a response without logging, and the server's own
// outer recover expects to see it, so intercepting it here would break
// legitimate stream aborts.
func RecoveryMiddleware(next http.Handler, group string, metrics *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				// Not a bug — a deliberate stream abort. Let it propagate.
				panic(rec)
			}

			var stack [8192]byte
			n := runtime.Stack(stack[:], false)
			logging.FromContext(r.Context()).Error("recovered from panic in request handler",
				"component", "dataplane",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(stack[:n]),
			)
			if metrics != nil {
				metrics.ObservePanic(group)
			}

			// Write a 500 if the response header hasn't been committed yet.
			// If it has (the handler panicked mid-stream), WriteHeader is a
			// no-op; guard the error write itself so a panicking
			// ResponseWriter can't re-panic out of the recover.
			defer func() { _ = recover() }()
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()

		next.ServeHTTP(w, r)
	})
}

// MaxConnsMiddleware bounds the number of requests served concurrently by
// next to maxConns. A request that arrives while maxConns are already in
// flight is rejected immediately with 503 (Retry-After: 1) rather than
// queued, so a connection flood cannot grow the process's memory without
// bound. maxConns <= 0 disables the limit (next is returned unwrapped).
//
// The semaphore is a buffered channel: a token is taken on entry and
// returned on exit (via defer, so it is released even if the handler
// panics — RecoveryMiddleware, which wraps the OUTSIDE of this, then turns
// that panic into a 500). A non-blocking send admits, so an over-limit
// request fails fast instead of parking a goroutine.
func MaxConnsMiddleware(next http.Handler, maxConns int) http.Handler {
	if maxConns <= 0 {
		return next
	}
	sem := make(chan struct{}, maxConns)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "server at connection limit, try again shortly", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MaxBodyMiddleware caps the number of bytes that can be read from each
// request body to maxBytes, using http.MaxBytesReader. Reading past the
// cap makes the body reader return an error and the server close the
// connection, so an oversized upload is stopped at the proxy rather than
// streamed to (and buffered by) a backend. maxBytes <= 0 disables the
// limit (next is returned unwrapped).
//
// It is a handler decorator (rather than a server setting) because Go has
// no server-level request-body cap; MaxBytesReader must be installed
// per-request on r.Body, which is exactly what this does before delegating.
func MaxBodyMiddleware(next http.Handler, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}
