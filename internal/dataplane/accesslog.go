// Package dataplane: accesslog.go adds per-request access logging and
// request-ID tracing to the L7 HTTP proxy, as a middleware that wraps the
// proxy handler.
//
// It is deliberately separate from the Prometheus/admin-UI metrics
// (metrics.go): metrics answer "how much / how fast in aggregate", an
// access log answers "what happened to this one request" — the two are
// complementary and read by different audiences (a scrape/dashboard vs.
// a human tailing logs or a log-aggregation pipeline).
//
// Request tracing: every request is stamped with a request ID. An
// incoming X-Request-ID header is honoured (so an ID assigned by an
// upstream edge/CDN flows through unchanged and correlates across hops);
// otherwise a fresh random ID is generated. The ID is echoed back on the
// response (X-Request-ID) and forwarded to the backend, and it appears in
// every access-log line for the request, so a single request can be
// traced client -> proxy -> backend by grepping one value.
package dataplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Josephfell/Jbalance/internal/logging"
)

// RequestIDHeader is the header carrying the request-trace ID, both
// inbound (honoured if present) and outbound (always set on the response
// and forwarded to the backend).
const RequestIDHeader = "X-Request-Id"

// AccessLogFormat selects how each access-log line is rendered.
type AccessLogFormat string

const (
	// AccessLogJSON emits one JSON object per request (best for log
	// aggregation / structured querying).
	AccessLogJSON AccessLogFormat = "json"
	// AccessLogText emits a compact human-readable line per request.
	AccessLogText AccessLogFormat = "text"
)

// AccessLogConfig configures per-request access logging.
type AccessLogConfig struct {
	// Enabled turns access logging on. When false, the middleware still
	// handles request-ID tracing (a cheap, always-useful correlation aid)
	// but writes no per-request log line.
	Enabled bool
	// Format is "json" (default) or "text".
	Format AccessLogFormat
}

// requestInfo carries the per-request facts the proxy discovers (chosen
// backend, retry count) back out to the access-log middleware, which sees
// them once the handler it wraps returns. A pointer to it is stored in
// the request context so the proxy can fill it in in place without the
// middleware and proxy needing to share any other state.
type requestInfo struct {
	backend string // address of the backend the request was finally sent to
	retries int    // number of retry attempts beyond the first
	group   string // resolved backend group
}

// requestInfoKey stores a *requestInfo in the request context.
const requestInfoKey contextKey = iota + 3

// requestInfoFrom returns the *requestInfo stored in ctx, or nil if the
// request did not pass through the access-log middleware (e.g. in a unit
// test exercising the proxy handler directly).
func requestInfoFrom(ctx context.Context) *requestInfo {
	ri, _ := ctx.Value(requestInfoKey).(*requestInfo)
	return ri
}

// recordBackend is called by the proxy to note the backend a request was
// finally proxied to and how many retries it took. Safe to call when the
// request did not pass through the middleware (no-op in that case).
func recordBackend(ctx context.Context, group, backend string, retries int) {
	if ri := requestInfoFrom(ctx); ri != nil {
		ri.group = group
		ri.backend = backend
		ri.retries = retries
	}
}

// accessLogResponseWriter wraps http.ResponseWriter to capture the status
// code and byte count actually written to the client, which an access log
// needs but the bare ResponseWriter doesn't expose after the fact.
type accessLogResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *accessLogResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // first Write with no explicit WriteHeader implies 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher when the underlying writer does, so
// streaming/SSE responses proxied through still flush to the client.
func (w *accessLogResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLogMiddleware wraps next with request-ID tracing and (if enabled)
// per-request access logging. It:
//
//   - honours an inbound X-Request-ID or generates one, echoing it back
//     on the response and forwarding it to the backend;
//   - installs a *requestInfo in the request context that the proxy fills
//     in with the chosen backend and retry count;
//   - after next returns, writes one access-log line capturing method,
//     path, status, bytes, latency, client IP, group, backend, retries,
//     and request ID.
//
// logger may be nil, in which case the standard logger is used, for the
// per-request access-log line (kept in its own JSON/text rendering for
// stability). appLog is the process-wide structured logger; the
// middleware stores a request-scoped child of it — tagged with the
// request's request_id — in the request context, so any component
// downstream (e.g. the proxy) that calls logging.FromContext(ctx) emits
// application log lines already correlated to this request. appLog may be
// nil, in which case the slog default is used. cfg controls whether an
// access-log line is written and in what format.
func AccessLogMiddleware(next http.Handler, cfg AccessLogConfig, logger *log.Logger, appLog *slog.Logger) http.Handler {
	if logger == nil {
		logger = log.New(os.Stdout, "", 0)
	}
	if appLog == nil {
		appLog = slog.Default()
	}
	format := cfg.Format
	if format != AccessLogText {
		format = AccessLogJSON
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		reqID := r.Header.Get(RequestIDHeader)
		if reqID == "" {
			reqID = newRequestID()
		}
		// Forward the ID to the backend and echo it to the client.
		r.Header.Set(RequestIDHeader, reqID)
		w.Header().Set(RequestIDHeader, reqID)

		ri := &requestInfo{}
		ctx := context.WithValue(r.Context(), requestInfoKey, ri)
		// Correlate every application log line emitted while serving this
		// request with its request_id, via a request-scoped logger placed
		// in the context.
		ctx = logging.WithContext(ctx, appLog.With(slog.String("request_id", reqID)))
		r = r.WithContext(ctx)

		alw := &accessLogResponseWriter{ResponseWriter: w}
		next.ServeHTTP(alw, r)

		if !cfg.Enabled {
			return
		}

		status := alw.status
		if status == 0 {
			status = http.StatusOK
		}
		writeAccessLog(logger, format, accessLogEntry{
			Time:       start.UTC().Format(time.RFC3339Nano),
			RequestID:  reqID,
			Method:     r.Method,
			Host:       r.Host,
			Path:       r.URL.Path,
			Status:     status,
			Bytes:      alw.bytes,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
			ClientIP:   clientIP(r),
			Group:      ri.group,
			Backend:    ri.backend,
			Retries:    ri.retries,
			Proto:      r.Proto,
			UserAgent:  r.UserAgent(),
		})
	})
}

// accessLogEntry is one request's worth of log fields.
type accessLogEntry struct {
	Time       string  `json:"time"`
	RequestID  string  `json:"request_id"`
	Method     string  `json:"method"`
	Host       string  `json:"host"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	Bytes      int64   `json:"bytes"`
	DurationMs float64 `json:"duration_ms"`
	ClientIP   string  `json:"client_ip"`
	Group      string  `json:"group"`
	Backend    string  `json:"backend"`
	Retries    int     `json:"retries"`
	Proto      string  `json:"proto"`
	UserAgent  string  `json:"user_agent"`
}

func writeAccessLog(logger *log.Logger, format AccessLogFormat, e accessLogEntry) {
	if format == AccessLogText {
		logger.Printf("access %s %s %q %s %d %dB %.1fms client=%s group=%s backend=%s retries=%d id=%s",
			e.Proto, e.Method, e.Host+e.Path, "->", e.Status, e.Bytes, e.DurationMs,
			e.ClientIP, e.Group, e.Backend, e.Retries, e.RequestID)
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		logger.Printf("access log: failed to marshal entry: %v", err)
		return
	}
	logger.Printf("%s", b)
}

// clientIP extracts the client address for logging: it prefers the
// left-most X-Forwarded-For entry when present (the original client
// behind a trusted upstream proxy/CDN), falling back to the direct
// RemoteAddr host. This mirrors how an operator would expect the "who
// made this request" field to read behind an edge.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Left-most entry is the original client; trim to the first comma.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return trimSpace(xff[:i])
			}
		}
		return trimSpace(xff)
	}
	// RemoteAddr is host:port; strip the port for a cleaner log field.
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// newRequestID returns a random 128-bit hex request ID. crypto/rand is
// used so IDs are unpredictable (they can appear in client-visible
// responses and logs); a failure to read randomness falls back to a
// timestamp-based value rather than returning an empty ID.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ts-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
