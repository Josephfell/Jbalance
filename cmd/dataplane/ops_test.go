package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpsMux_Healthz confirms liveness is always 200 regardless of
// readiness — it must only report process aliveness, so an
// orchestrator's liveness probe never restarts a pod merely because it
// has no healthy backends yet.
func TestOpsMux_Healthz(t *testing.T) {
	for _, ready := range []bool{true, false} {
		mux := opsMux(func() bool { return ready })
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("ready=%v: /healthz status=%d, want 200", ready, rec.Code)
		}
		if body := rec.Body.String(); body != "ok" {
			t.Errorf("ready=%v: /healthz body=%q, want %q", ready, body, "ok")
		}
	}
}

// TestOpsMux_Readyz confirms readiness reflects the ready() closure:
// 200 when ready, 503 when not (so the instance is pulled out of the
// service without a restart), and 503 for a nil closure.
func TestOpsMux_Readyz(t *testing.T) {
	tests := []struct {
		name     string
		ready    func() bool
		wantCode int
	}{
		{"ready", func() bool { return true }, http.StatusOK},
		{"not ready", func() bool { return false }, http.StatusServiceUnavailable},
		{"nil closure", nil, http.StatusServiceUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := opsMux(tc.ready)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tc.wantCode {
				t.Errorf("/readyz status=%d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

// TestOpsMux_ReadyzReflectsLiveState confirms /readyz recomputes on every
// request, so a backend recovering flips it from 503 back to 200 with no
// caching.
func TestOpsMux_ReadyzReflectsLiveState(t *testing.T) {
	healthy := false
	mux := opsMux(func() bool { return healthy })

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial /readyz status=%d, want 503", rec.Code)
	}

	healthy = true
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("after recovery /readyz status=%d, want 200", rec.Code)
	}
}
