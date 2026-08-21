package dataplane

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/Josephfell/Jbalance/proto"
)

// fakeControlPlane implements pb.ControlPlaneServer, recording every
// ReportHealth call it receives so tests can assert on what was sent.
type fakeControlPlane struct {
	pb.UnimplementedControlPlaneServer
	received chan *pb.HealthReport
}

func (f *fakeControlPlane) ReportHealth(_ context.Context, report *pb.HealthReport) (*pb.HealthReportAck, error) {
	f.received <- report
	return &pb.HealthReportAck{}, nil
}

func startFakeControlPlane(t *testing.T) (addr string, received chan *pb.HealthReport, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	fake := &fakeControlPlane{received: make(chan *pb.HealthReport, 8)}
	grpcServer := grpc.NewServer()
	pb.RegisterControlPlaneServer(grpcServer, fake)

	go func() { _ = grpcServer.Serve(lis) }()

	return lis.Addr().String(), fake.received, func() {
		grpcServer.Stop()
		_ = lis.Close()
	}
}

func TestHealthReporter_SendsCurrentHealthSnapshot(t *testing.T) {
	addr, received, stop := startFakeControlPlane(t)
	defer stop()

	backends := NewBackendList()
	backends.Update(&pb.BackendSet{
		Group:   "web-tier",
		Version: 1,
		Backends: []*pb.Backend{
			{Address: "a:1", Weight: 1},
			{Address: "b:1", Weight: 1},
		},
	})
	backends.SetHealth("b:1", false)

	reporter := NewHealthReporter(addr, "web-tier", "dp-test", backends, nil, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go reporter.Run(ctx)

	select {
	case report := <-received:
		if report.Group != "web-tier" {
			t.Errorf("expected group 'web-tier', got %q", report.Group)
		}
		if report.InstanceId != "dp-test" {
			t.Errorf("expected instance_id 'dp-test', got %q", report.InstanceId)
		}
		if len(report.Backends) != 2 {
			t.Fatalf("expected 2 backends in report, got %d", len(report.Backends))
		}
		health := map[string]bool{}
		for _, b := range report.Backends {
			health[b.Address] = b.Healthy
		}
		if !health["a:1"] {
			t.Error("expected a:1 to be reported healthy")
		}
		if health["b:1"] {
			t.Error("expected b:1 to be reported unhealthy")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a health report")
	}
}

func TestHealthReporter_SendsNothingWhenNoBackendsKnown(t *testing.T) {
	addr, received, stop := startFakeControlPlane(t)
	defer stop()

	backends := NewBackendList() // empty — no Update called yet

	reporter := NewHealthReporter(addr, "web-tier", "dp-test", backends, nil, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go reporter.Run(ctx)

	select {
	case <-received:
		t.Fatal("expected no report to be sent when there are no known backends")
	case <-ctx.Done():
		// expected — no report arrived within the test window
	}
}

func TestHealthReporter_ContinuesAfterConnectionFailure(t *testing.T) {
	// Point at a port nothing is listening on — Run should log and return
	// without panicking rather than blocking forever or crashing.
	backends := NewBackendList()
	backends.Update(&pb.BackendSet{Group: "g", Version: 1, Backends: []*pb.Backend{{Address: "a:1", Weight: 1}}})

	reporter := NewHealthReporter("127.0.0.1:1", "g", "dp-test", backends, nil, 20*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		reporter.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Run returned cleanly — good, no hang/panic.
	case <-time.After(2 * time.Second):
		t.Fatal("HealthReporter.Run did not return after context cancellation")
	}
}
