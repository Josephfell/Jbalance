package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/josephfell/go-loadbalancer/internal/pool"
	pb "github.com/josephfell/go-loadbalancer/proto"
)

func TestReportHealth_UpdatesHealthState(t *testing.T) {
	srv := NewServer(nil, nil, nil)
	srv.publishIfChanged("g1", pool.Snapshot{
		Group:    "g1",
		Backends: []pool.Backend{{Address: "a:1", Weight: 1}},
	})

	_, err := srv.ReportHealth(context.Background(), &pb.HealthReport{
		Group:      "g1",
		InstanceId: "dp-1",
		Backends:   []*pb.BackendHealth{{Address: "a:1", Healthy: true}},
	})
	if err != nil {
		t.Fatalf("ReportHealth() error: %v", err)
	}

	healthy, known := srv.healthForAddress("g1", "a:1")
	if !known {
		t.Fatal("expected health to be known after a report")
	}
	if !healthy {
		t.Error("expected the backend to be reported healthy")
	}
}

func TestHealthForAddress_UnknownBeforeAnyReport(t *testing.T) {
	srv := NewServer(nil, nil, nil)
	_, known := srv.healthForAddress("g1", "a:1")
	if known {
		t.Error("expected health to be unknown before any report is received")
	}
}

func TestHealthForAddress_UnhealthyIfAnyReporterDisagrees(t *testing.T) {
	srv := NewServer(nil, nil, nil)

	_, _ = srv.ReportHealth(context.Background(), &pb.HealthReport{
		Group:      "g1",
		InstanceId: "dp-1",
		Backends:   []*pb.BackendHealth{{Address: "a:1", Healthy: true}},
	})
	_, _ = srv.ReportHealth(context.Background(), &pb.HealthReport{
		Group:      "g1",
		InstanceId: "dp-2",
		Backends:   []*pb.BackendHealth{{Address: "a:1", Healthy: false}},
	})

	healthy, known := srv.healthForAddress("g1", "a:1")
	if !known {
		t.Fatal("expected health to be known")
	}
	if healthy {
		t.Error("expected the backend to be considered unhealthy when any reporter disagrees")
	}
}

func TestHealthForAddress_StaleReportsAreIgnored(t *testing.T) {
	srv := NewServer(nil, nil, nil)

	srv.healthMu.Lock()
	srv.health[backendHealthKey{group: "g1", instanceID: "dp-1", address: "a:1"}] = healthEntry{
		healthy:    true,
		reportedAt: time.Now().Add(-time.Hour), // long past healthReportMaxAge
	}
	srv.healthMu.Unlock()

	_, known := srv.healthForAddress("g1", "a:1")
	if known {
		t.Error("expected a stale report to be treated as unknown")
	}
}

func TestHealthForAddress_ScopedToGroup(t *testing.T) {
	srv := NewServer(nil, nil, nil)

	_, _ = srv.ReportHealth(context.Background(), &pb.HealthReport{
		Group:      "g1",
		InstanceId: "dp-1",
		Backends:   []*pb.BackendHealth{{Address: "a:1", Healthy: true}},
	})

	_, known := srv.healthForAddress("g2", "a:1")
	if known {
		t.Error("expected a report for group g1 not to leak into group g2's health lookup")
	}
}

func TestSnapshot_IncludesHealthState(t *testing.T) {
	provider := &stubProvider{
		groups: []string{"g1"},
		snapshots: map[string]pool.Snapshot{
			"g1": {Group: "g1", Backends: []pool.Backend{{Address: "a:1", Weight: 1}, {Address: "b:1", Weight: 1}}},
		},
	}
	srv := NewServer(provider, nil, nil)
	srv.publishIfChanged("g1", provider.snapshots["g1"])
	_, _ = srv.ReportHealth(context.Background(), &pb.HealthReport{
		Group:      "g1",
		InstanceId: "dp-1",
		Backends:   []*pb.BackendHealth{{Address: "a:1", Healthy: true}},
	})

	states := srv.Snapshot(context.Background())
	if len(states) != 1 {
		t.Fatalf("expected 1 group, got %d", len(states))
	}

	var foundA, foundB bool
	for _, b := range states[0].Backends {
		if b.Address == "a:1" {
			foundA = true
			if !b.HealthKnown || !b.Healthy {
				t.Errorf("expected a:1 to be known-healthy, got %+v", b)
			}
		}
		if b.Address == "b:1" {
			foundB = true
			if b.HealthKnown {
				t.Errorf("expected b:1 to have unknown health (no report received), got %+v", b)
			}
		}
	}
	if !foundA || !foundB {
		t.Fatal("expected both backends to be present in the snapshot")
	}
}

// stubProvider is a minimal pool.Provider for tests that exercise
// Snapshot(ctx), which (unlike publishIfChanged) reads directly from the
// provider rather than from the server's last-published state.
type stubProvider struct {
	groups    []string
	snapshots map[string]pool.Snapshot
}

func (p *stubProvider) Groups(_ context.Context) ([]string, error) {
	return p.groups, nil
}

func (p *stubProvider) Snapshot(_ context.Context, group string) (pool.Snapshot, error) {
	return p.snapshots[group], nil
}
