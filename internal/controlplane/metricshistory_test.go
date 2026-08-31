package controlplane

import (
	"testing"
	"time"
)

func TestMetricsHistory_RecordsAndReturnsRecent(t *testing.T) {
	h := newMetricsHistory()
	h.record([]GroupMetricsSnapshot{
		{Group: "web-tier", RequestsTotal: 100, Errors5xxTotal: 2, ActiveConnections: 5, AvgDurationMs: 12},
		{Group: "api-tier", RequestsTotal: 50, Errors5xxTotal: 1, ActiveConnections: 3, AvgDurationMs: 8},
	})

	points := h.Recent(10)
	if len(points) != 1 {
		t.Fatalf("expected 1 point, got %d", len(points))
	}
	p := points[0]
	if p.ActiveConns != 8 {
		t.Errorf("expected active connections summed to 8, got %d", p.ActiveConns)
	}
	if p.AvgDurationMs != 10 {
		t.Errorf("expected average duration (12+8)/2=10, got %v", p.AvgDurationMs)
	}
}

func TestMetricsHistory_DeltasComputedBetweenPoints(t *testing.T) {
	h := newMetricsHistory()
	past := time.Now().Add(-time.Hour)

	h.mu.Lock()
	h.points = append(h.points,
		historyPoint{t: past, requestsTotal: 100, errors5xxTotal: 2},
		historyPoint{t: past.Add(historyBucketWidth), requestsTotal: 150, errors5xxTotal: 5},
	)
	h.mu.Unlock()

	points := h.Recent(10)
	if len(points) != 2 {
		t.Fatalf("expected 2 points, got %d", len(points))
	}
	if points[0].RequestsDelta != 0 {
		t.Errorf("expected the first point's delta to be 0 (no predecessor), got %d", points[0].RequestsDelta)
	}
	if points[1].RequestsDelta != 50 {
		t.Errorf("expected the second point's delta to be 150-100=50, got %d", points[1].RequestsDelta)
	}
	if points[1].Errors5xxDelta != 3 {
		t.Errorf("expected the second point's error delta to be 5-2=3, got %d", points[1].Errors5xxDelta)
	}
}

func TestMetricsHistory_NegativeDeltaClampedToZero(t *testing.T) {
	h := newMetricsHistory()
	past := time.Now().Add(-time.Hour)

	h.mu.Lock()
	h.points = append(h.points,
		historyPoint{t: past, requestsTotal: 500},
		// Simulates an instance restart resetting its cumulative counter.
		historyPoint{t: past.Add(historyBucketWidth), requestsTotal: 10},
	)
	h.mu.Unlock()

	points := h.Recent(10)
	if points[1].RequestsDelta != 0 {
		t.Errorf("expected a counter reset to clamp the delta to 0, got %d", points[1].RequestsDelta)
	}
}

func TestMetricsHistory_UpdatesCurrentBucketWithinWindow(t *testing.T) {
	h := newMetricsHistory()
	h.record([]GroupMetricsSnapshot{{Group: "g1", RequestsTotal: 10}})
	h.record([]GroupMetricsSnapshot{{Group: "g1", RequestsTotal: 20}}) // within historyBucketWidth of the first call

	h.mu.Lock()
	n := len(h.points)
	h.mu.Unlock()
	if n != 1 {
		t.Errorf("expected both record calls within the bucket width to coalesce into 1 point, got %d", n)
	}
}

func TestMetricsHistory_RecentRespectsLimit(t *testing.T) {
	h := newMetricsHistory()
	now := time.Now()
	h.mu.Lock()
	for i := 0; i < 5; i++ {
		h.points = append(h.points, historyPoint{t: now.Add(time.Duration(i) * historyBucketWidth), requestsTotal: int64(i * 10)})
	}
	h.mu.Unlock()

	got := h.Recent(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 points, got %d", len(got))
	}
}

func TestMetricsHistory_BoundedByMaxPoints(t *testing.T) {
	h := newMetricsHistory()
	past := time.Now().Add(-time.Hour)
	h.mu.Lock()
	for i := 0; i < maxHistoryPoints; i++ {
		h.points = append(h.points, historyPoint{t: past.Add(time.Duration(i) * historyBucketWidth)})
	}
	h.mu.Unlock()

	h.record([]GroupMetricsSnapshot{{Group: "g1", RequestsTotal: 1}})

	h.mu.Lock()
	n := len(h.points)
	h.mu.Unlock()
	if n > maxHistoryPoints {
		t.Errorf("expected the history to be bounded to %d points, got %d", maxHistoryPoints, n)
	}
}

func TestMetricsHistory_EmptyWithNoSnapshots(t *testing.T) {
	h := newMetricsHistory()
	h.record(nil)

	points := h.Recent(10)
	if len(points) != 1 {
		t.Fatalf("expected recording an empty snapshot list to still produce a (zeroed) point, got %d", len(points))
	}
	if points[0].ActiveConns != 0 || points[0].AvgDurationMs != 0 {
		t.Errorf("expected a zeroed point for no data, got %+v", points[0])
	}
}
