package dataplane

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogMiddleware_GeneratesAndEchoesRequestID(t *testing.T) {
	var gotBackendHeader string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBackendHeader = r.Header.Get(RequestIDHeader) // forwarded to backend
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: false}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	echoed := rec.Header().Get(RequestIDHeader)
	if echoed == "" {
		t.Fatal("expected a generated X-Request-Id on the response, got none")
	}
	if gotBackendHeader != echoed {
		t.Fatalf("request ID forwarded to backend (%q) != echoed to client (%q)", gotBackendHeader, echoed)
	}
	if len(echoed) != 32 { // 16 random bytes hex-encoded
		t.Fatalf("expected a 32-char hex request ID, got %q (len %d)", echoed, len(echoed))
	}
}

func TestAccessLogMiddleware_HonoursInboundRequestID(t *testing.T) {
	const incoming = "trace-from-edge-123"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(RequestIDHeader); got != incoming {
			t.Errorf("backend saw request ID %q, want %q", got, incoming)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: false}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, incoming)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != incoming {
		t.Fatalf("response echoed request ID %q, want the inbound %q", got, incoming)
	}
}

func TestAccessLogMiddleware_JSONEntryCapturesRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate the proxy recording the chosen backend + one retry.
		recordBackend(r.Context(), "web-tier", "10.0.0.5:8080", 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream failed"))
	})

	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: true, Format: AccessLogJSON}, logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	req.RemoteAddr = "203.0.113.7:44321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected an access-log line, got none")
	}
	var e accessLogEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("access log line is not valid JSON: %v\nline: %s", err, line)
	}

	if e.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", e.Method)
	}
	if e.Path != "/api/orders" {
		t.Errorf("path = %q, want /api/orders", e.Path)
	}
	if e.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", e.Status)
	}
	if e.Bytes != int64(len("upstream failed")) {
		t.Errorf("bytes = %d, want %d", e.Bytes, len("upstream failed"))
	}
	if e.Group != "web-tier" {
		t.Errorf("group = %q, want web-tier", e.Group)
	}
	if e.Backend != "10.0.0.5:8080" {
		t.Errorf("backend = %q, want 10.0.0.5:8080", e.Backend)
	}
	if e.Retries != 1 {
		t.Errorf("retries = %d, want 1", e.Retries)
	}
	if e.ClientIP != "203.0.113.7" {
		t.Errorf("client_ip = %q, want 203.0.113.7 (port stripped)", e.ClientIP)
	}
	if e.RequestID == "" {
		t.Error("expected a request_id in the log entry")
	}
}

func TestAccessLogMiddleware_PrefersXForwardedForClientIP(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: true, Format: AccessLogJSON}, logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.1.1:9999" // the proxy in front of us
	req.Header.Set("X-Forwarded-For", "198.51.100.23, 10.1.1.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var e accessLogEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &e); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if e.ClientIP != "198.51.100.23" {
		t.Fatalf("client_ip = %q, want the left-most XFF entry 198.51.100.23", e.ClientIP)
	}
}

func TestAccessLogMiddleware_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordBackend(r.Context(), "api-tier", "10.0.0.9:8081", 0)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: true, Format: AccessLogText}, logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	line := buf.String()
	if !strings.Contains(line, "backend=10.0.0.9:8081") {
		t.Errorf("text line missing backend field: %s", line)
	}
	if !strings.Contains(line, "group=api-tier") {
		t.Errorf("text line missing group field: %s", line)
	}
	if !strings.Contains(line, "200") {
		t.Errorf("text line missing status: %s", line)
	}
}

func TestAccessLogMiddleware_Disabled_NoLine(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := AccessLogMiddleware(next, AccessLogConfig{Enabled: false}, logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if buf.Len() != 0 {
		t.Fatalf("expected no log output when disabled, got: %s", buf.String())
	}
	// But tracing must still work.
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Fatal("request-ID tracing should work even when access logging is disabled")
	}
}
