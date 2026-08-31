package dataplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestMetrics_ObserveHTTPRequest_ExposedViaPromhttp(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveHTTPRequest("web-tier", 200, 15*time.Millisecond)
	m.ObserveHTTPRequest("web-tier", 503, 2*time.Millisecond)

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `jbalance_http_requests_total{group="web-tier",status="2xx"} 1`) {
		t.Errorf("expected a 2xx request to be counted, got:\n%s", body)
	}
	if !strings.Contains(body, `jbalance_http_requests_total{group="web-tier",status="5xx"} 1`) {
		t.Errorf("expected a 5xx request to be counted, got:\n%s", body)
	}
	if !strings.Contains(body, "jbalance_http_request_duration_seconds") {
		t.Error("expected the request duration histogram to be present")
	}
}

func TestMetrics_ActiveConnections_GaugeTracksDelta(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.SetActiveConnections("web-tier", 1)
	m.SetActiveConnections("web-tier", 1)
	m.SetActiveConnections("web-tier", -1)

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `jbalance_active_connections{group="web-tier"} 1`) {
		t.Errorf("expected active connections gauge to reflect net +1, got:\n%s", body)
	}
}

func TestMetrics_TCPMetrics_ExposedViaPromhttp(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveTCPConnection("tcp-tier")
	m.SetTCPActiveConnections("tcp-tier", 1)
	m.ObserveTCPBytes("tcp-tier", "in", 100)
	m.ObserveTCPBytes("tcp-tier", "out", 250)

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `jbalance_tcp_connections_total{group="tcp-tier"} 1`) {
		t.Errorf("expected 1 TCP connection counted, got:\n%s", body)
	}
	if !strings.Contains(body, `jbalance_tcp_active_connections{group="tcp-tier"} 1`) {
		t.Errorf("expected 1 active TCP connection, got:\n%s", body)
	}
	if !strings.Contains(body, `jbalance_tcp_bytes_total{direction="in",group="tcp-tier"} 100`) {
		t.Errorf("expected 100 bytes in, got:\n%s", body)
	}
	if !strings.Contains(body, `jbalance_tcp_bytes_total{direction="out",group="tcp-tier"} 250`) {
		t.Errorf("expected 250 bytes out, got:\n%s", body)
	}
}

func TestMetrics_ObserveTCPBytes_IgnoresNonPositive(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveTCPBytes("tcp-tier", "in", 0)
	m.ObserveTCPBytes("tcp-tier", "in", -5)

	body := scrapeMetrics(t, reg)
	if strings.Contains(body, "jbalance_tcp_bytes_total") {
		t.Errorf("expected no bytes-total sample for zero/negative values, got:\n%s", body)
	}
}

func TestBackendsCollector_ReflectsGroupManagerLive(t *testing.T) {
	reg := prometheus.NewRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, HealthCheckConfig{
		Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2,
	}, time.Hour)

	bl := groups.Ensure("web-tier")
	bl.Update(&pb.BackendSet{Group: "web-tier", Version: 1, Backends: []*pb.Backend{
		{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1},
	}})
	bl.SetHealth("a:1", false)

	RegisterBackendsCollector(reg, groups)

	body := scrapeMetrics(t, reg)
	if !strings.Contains(body, `jbalance_backends_total{group="web-tier"} 2`) {
		t.Errorf("expected backends_total 2, got:\n%s", body)
	}
	if !strings.Contains(body, `jbalance_backends_healthy{group="web-tier"} 1`) {
		t.Errorf("expected backends_healthy 1, got:\n%s", body)
	}
}

func TestSeriesStore_RecordsAndReturnsRecentPoints(t *testing.T) {
	s := newSeriesStore()

	s.recordRequest("web-tier", "2xx", 10*time.Millisecond)
	s.recordRequest("web-tier", "5xx", 20*time.Millisecond)
	s.recordActiveConns("web-tier", 3)

	points := s.Recent(10)
	if len(points) != 1 {
		t.Fatalf("expected 1 bucket (all within the same bucket width), got %d", len(points))
	}
	p := points[0]
	if p.Requests != 2 {
		t.Errorf("expected 2 requests recorded, got %d", p.Requests)
	}
	if p.Errors5xx != 1 {
		t.Errorf("expected 1 5xx error recorded, got %d", p.Errors5xx)
	}
	if p.AvgDurationMs != 15 {
		t.Errorf("expected average duration 15ms, got %v", p.AvgDurationMs)
	}
	if p.ActiveConns != 3 {
		t.Errorf("expected active conns 3, got %d", p.ActiveConns)
	}
}

func TestSeriesStore_ActiveConnsNeverGoesNegative(t *testing.T) {
	s := newSeriesStore()
	s.recordActiveConns("web-tier", -5) // more decrements than increments should never have happened, but must not go negative

	points := s.Recent(1)
	if len(points) != 1 || points[0].ActiveConns != 0 {
		t.Errorf("expected active conns to clamp at 0, got %+v", points)
	}
}

func TestSeriesStore_TCPRecording(t *testing.T) {
	s := newSeriesStore()
	s.recordTCPConnection("tcp-tier")
	s.recordTCPConnection("tcp-tier")
	s.recordTCPBytes("tcp-tier", "in", 100)
	s.recordTCPBytes("tcp-tier", "out", 50)

	points := s.Recent(1)
	if len(points) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(points))
	}
	p := points[0]
	if p.TCPConns != 2 {
		t.Errorf("expected 2 TCP connections, got %d", p.TCPConns)
	}
	if p.TCPBytesIn != 100 || p.TCPBytesOut != 50 {
		t.Errorf("expected bytesIn=100 bytesOut=50, got in=%d out=%d", p.TCPBytesIn, p.TCPBytesOut)
	}
}

func TestSeriesStore_RecentRespectsLimit(t *testing.T) {
	s := newSeriesStore()
	// Force several distinct buckets by manipulating time directly on
	// stored points rather than sleeping in a test.
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.mu.Lock()
		s.points = append(s.points, seriesPoint{t: now.Add(time.Duration(i) * seriesBucketWidth), requests: int64(i)})
		s.mu.Unlock()
	}

	got := s.Recent(2)
	if len(got) != 2 {
		t.Fatalf("expected Recent(2) to return exactly 2 points, got %d", len(got))
	}
	// Must be the last two (oldest-first order preserved).
	if got[0].Requests != 3 || got[1].Requests != 4 {
		t.Errorf("expected the most recent 2 points in order, got %+v", got)
	}
}

func TestSeriesStore_BoundedByMaxPoints(t *testing.T) {
	s := newSeriesStore()
	// Seed exactly at the bound with points timestamped in the past (not
	// the future), so the next recordRequest call's currentBucketLocked
	// sees the last bucket as aged-out and actually appends (and
	// therefore trims back down to the bound) rather than reusing it.
	// In real usage the store only ever grows one point at a time via
	// this same append-then-trim-by-one path, so it can never exceed the
	// bound in the first place — this seeds the boundary condition
	// directly rather than needing maxSeriesPoints real ticks to reach it.
	past := time.Now().Add(-time.Hour)
	s.mu.Lock()
	for i := 0; i < maxSeriesPoints; i++ {
		s.points = append(s.points, seriesPoint{t: past.Add(time.Duration(i) * seriesBucketWidth)})
	}
	s.mu.Unlock()

	s.recordRequest("g", "2xx", time.Millisecond)

	s.mu.Lock()
	n := len(s.points)
	s.mu.Unlock()
	if n > maxSeriesPoints {
		t.Errorf("expected the series to be bounded to %d points, got %d", maxSeriesPoints, n)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{100: "1xx", 200: "2xx", 301: "3xx", 404: "4xx", 503: "5xx"}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// scrapeMetrics renders reg's current metrics via the real promhttp
// handler, so tests assert against the actual exposition format rather
// than reaching into internal collector state.
func scrapeMetrics(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
