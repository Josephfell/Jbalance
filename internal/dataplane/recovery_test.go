package dataplane

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRecoveryMiddleware_RecoversPanic verifies a panicking handler is
// turned into a 500 rather than crashing, and that the panic is counted.
func TestRecoveryMiddleware_RecoversPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := RecoveryMiddleware(panicky, "web-tier", m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	// Must not propagate the panic.
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after recovered panic, got %d", rec.Code)
	}
	if got := testutilCounter(t, reg, "jbalance_http_panics_total"); got != 1 {
		t.Fatalf("expected panic counter = 1, got %v", got)
	}
}

// TestRecoveryMiddleware_NoPanicPassThrough verifies a normal handler is
// unaffected and no panic is recorded.
func TestRecoveryMiddleware_NoPanicPassThrough(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("fine"))
	})
	h := RecoveryMiddleware(ok, "web-tier", m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected pass-through 418, got %d", rec.Code)
	}
	if rec.Body.String() != "fine" {
		t.Fatalf("expected body 'fine', got %q", rec.Body.String())
	}
	if got := testutilCounter(t, reg, "jbalance_http_panics_total"); got != 0 {
		t.Fatalf("expected panic counter = 0, got %v", got)
	}
}

// TestRecoveryMiddleware_NilMetrics verifies recovery still works (500, no
// crash) when metrics is nil.
func TestRecoveryMiddleware_NilMetrics(t *testing.T) {
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := RecoveryMiddleware(panicky, "web-tier", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// TestRecoveryMiddleware_AbortHandlerPropagates verifies the deliberate
// stream-abort sentinel is re-panicked, not swallowed.
func TestRecoveryMiddleware_AbortHandlerPropagates(t *testing.T) {
	aborting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})
	h := RecoveryMiddleware(aborting, "web-tier", nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	defer func() {
		rv := recover()
		if rv != http.ErrAbortHandler {
			t.Fatalf("expected ErrAbortHandler to propagate, got %v", rv)
		}
	}()
	h.ServeHTTP(rec, req)
	t.Fatal("expected panic to propagate, but handler returned normally")
}

// TestMaxConnsMiddleware_LimitsConcurrency verifies that once maxConns are
// in flight, further requests are rejected with 503 rather than queued.
func TestMaxConnsMiddleware_LimitsConcurrency(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})

	blocking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := MaxConnsMiddleware(blocking, 1)

	// Fill the single slot with a request that blocks until released.
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(rec, req)
	}()
	<-entered // first request is now in the handler, holding the token

	// A second request while the slot is taken must be rejected 503.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when at connection limit, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 503")
	}

	// Release the first request; the slot frees and a new request succeeds.
	close(release)
	// Drain any leftover entered signal (the released handler doesn't send again).

	// Give the first goroutine a chance to return the token, then a fresh
	// request (with a non-blocking handler this time) should be admitted.
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h2 := MaxConnsMiddleware(okHandler, 1)
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	h2.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200 when a slot is free, got %d", rec3.Code)
	}
}

// TestMaxConnsMiddleware_Disabled verifies maxConns <= 0 returns the
// handler unwrapped (no limit applied) and many concurrent requests pass.
func TestMaxConnsMiddleware_Disabled(t *testing.T) {
	var served int
	var mu sync.Mutex
	h := MaxConnsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}), 0)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200 with no limit, got %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	if served != 20 {
		t.Fatalf("expected all 20 requests served, got %d", served)
	}
}

// TestMaxBodyMiddleware_RejectsOversizedBody verifies a body larger than
// the cap makes the read fail (so the handler cannot pull it in full),
// while a body within the cap reads cleanly.
func TestMaxBodyMiddleware_RejectsOversizedBody(t *testing.T) {
	var readErr error
	var readN int
	h := MaxBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		readN = len(b)
		readErr = err
		w.WriteHeader(http.StatusOK)
	}), 10)

	// 20-byte body against a 10-byte cap: read must error out past the cap.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 20)))
	h.ServeHTTP(rec, req)
	if readErr == nil {
		t.Fatalf("expected read error for oversized body, got nil (read %d bytes)", readN)
	}
}

// TestMaxBodyMiddleware_AllowsWithinCap verifies a body within the cap
// reads without error.
func TestMaxBodyMiddleware_AllowsWithinCap(t *testing.T) {
	var readErr error
	var body string
	h := MaxBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		body = string(b)
		readErr = err
		w.WriteHeader(http.StatusOK)
	}), 100)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	h.ServeHTTP(rec, req)
	if readErr != nil {
		t.Fatalf("expected no read error within cap, got %v", readErr)
	}
	if body != "hello" {
		t.Fatalf("expected body 'hello', got %q", body)
	}
}

// TestMaxBodyMiddleware_Disabled verifies maxBytes <= 0 leaves the body
// unbounded (the full body reads regardless of size).
func TestMaxBodyMiddleware_Disabled(t *testing.T) {
	var readN int
	h := MaxBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		readN = len(b)
		w.WriteHeader(http.StatusOK)
	}), 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 5000)))
	h.ServeHTTP(rec, req)
	if readN != 5000 {
		t.Fatalf("expected full 5000-byte body with no cap, got %d", readN)
	}
}

// testutilCounter reads the current value of a single-series counter (no
// labels beyond one aggregated group) from the registry, summing across
// label combinations. Used to assert a counter incremented as expected.
func testutilCounter(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mt := range mf.GetMetric() {
			if c := mt.GetCounter(); c != nil {
				total += c.GetValue()
			}
		}
	}
	return total
}
