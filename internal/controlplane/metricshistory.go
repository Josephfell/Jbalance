package controlplane

import (
	"sync"
	"time"
)

// metricsHistory is a small in-memory ring buffer of aggregated fleet-wide
// traffic samples, for the admin web UI's overview chart. Deliberately
// not per-group (GroupMetricsSnapshot/MetricsSnapshot already gives a
// per-group table for "right now") — the chart is meant to answer "what's
// happening across everything this control plane manages", the same
// at-a-glance aggregate view the dashboard's other summary numbers give.
//
// Sampled once per ReportMetrics call rather than on its own timer: with
// multiple data plane instances reporting on their own independent
// intervals, samples naturally land close together in wall-clock time
// and the bucket-by-time-window logic (mirroring dataplane's seriesStore)
// coalesces them, so this doesn't need a separate background goroutine
// or the added lifecycle complexity of one.
type metricsHistory struct {
	mu     sync.Mutex
	points []historyPoint
}

type historyPoint struct {
	t                 time.Time
	requestsTotal     int64 // cumulative, as last observed in this bucket
	errors5xxTotal    int64 // cumulative, as last observed in this bucket
	activeConnections int64
	avgDurationMs     float64
}

// maxHistoryPoints/historyBucketWidth mirror dataplane's seriesStore
// bounds/rationale exactly — same local, no-external-database pattern,
// same "a bit over 20 minutes of history is enough for an at-a-glance
// chart" reasoning.
const (
	maxHistoryPoints   = 250
	historyBucketWidth = 5 * time.Second
)

func newMetricsHistory() *metricsHistory {
	return &metricsHistory{points: make([]historyPoint, 0, maxHistoryPoints)}
}

// record appends (or updates, if the current time bucket hasn't aged out
// yet) a sample built from an aggregated snapshot across every group.
func (h *metricsHistory) record(snapshots []GroupMetricsSnapshot) {
	var totalRequests, totalErrors, totalActive int64
	var durationSum float64
	var groupsWithData int
	for _, g := range snapshots {
		totalRequests += g.RequestsTotal
		totalErrors += g.Errors5xxTotal
		totalActive += g.ActiveConnections
		durationSum += g.AvgDurationMs
		groupsWithData++
	}
	avgDuration := 0.0
	if groupsWithData > 0 {
		avgDuration = durationSum / float64(groupsWithData)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	if len(h.points) > 0 && now.Sub(h.points[len(h.points)-1].t) < historyBucketWidth {
		p := &h.points[len(h.points)-1]
		p.requestsTotal = totalRequests
		p.errors5xxTotal = totalErrors
		p.activeConnections = totalActive
		p.avgDurationMs = avgDuration
		return
	}

	h.points = append(h.points, historyPoint{
		t: now, requestsTotal: totalRequests, errors5xxTotal: totalErrors,
		activeConnections: totalActive, avgDurationMs: avgDuration,
	})
	if len(h.points) > maxHistoryPoints {
		h.points = h.points[1:]
	}
}

// HistoryPoint is the read-only, display-ready form of one sample.
// RequestsDelta/Errors5xxDelta are computed relative to the previous
// point (both underlying counters are cumulative-since-instance-start
// sums, not per-bucket counts) — Recent does this math so template code
// doesn't have to.
type HistoryPoint struct {
	Time           time.Time
	RequestsDelta  int64
	Errors5xxDelta int64
	ActiveConns    int64
	AvgDurationMs  float64
}

// Recent returns up to the last n samples, oldest first, with delta
// fields computed against each point's predecessor. The very first
// returned point's deltas are always 0 (no predecessor available within
// the returned window) — acceptable for a live-scrolling chart where
// that point ages out within one bucket width anyway.
func (h *metricsHistory) Recent(n int) []HistoryPoint {
	h.mu.Lock()
	defer h.mu.Unlock()

	start := 0
	if len(h.points) > n {
		start = len(h.points) - n
	}
	src := h.points[start:]

	out := make([]HistoryPoint, len(src))
	for i, p := range src {
		var reqDelta, errDelta int64
		if i > 0 {
			reqDelta = p.requestsTotal - src[i-1].requestsTotal
			errDelta = p.errors5xxTotal - src[i-1].errors5xxTotal
		} else if start > 0 {
			// The point immediately before this window still exists in
			// the underlying buffer — use it so the first visible point
			// in a scrolled-forward window isn't artificially zeroed.
			reqDelta = p.requestsTotal - h.points[start-1].requestsTotal
			errDelta = p.errors5xxTotal - h.points[start-1].errors5xxTotal
		}
		if reqDelta < 0 {
			reqDelta = 0 // a counter reset (e.g. instance restart) must not show as negative traffic
		}
		if errDelta < 0 {
			errDelta = 0
		}
		out[i] = HistoryPoint{
			Time:           p.t,
			RequestsDelta:  reqDelta,
			Errors5xxDelta: errDelta,
			ActiveConns:    p.activeConnections,
			AvgDurationMs:  p.avgDurationMs,
		}
	}
	return out
}
