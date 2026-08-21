package dataplane

import (
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// listenOnce starts a TCP listener that accepts and immediately closes
// connections, just enough to make a connect-based health check succeed.
// Returns the address and a function to stop listening (simulating the
// backend going down).
func listenOnce(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start test listener: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	go func() {
		<-done
		ln.Close()
	}()

	return ln.Addr().String(), func() { close(done) }
}

func TestHealthChecker_MarksUnreachableBackendUnhealthy(t *testing.T) {
	bl := NewBackendList()
	// A port nothing is listening on — connect should fail immediately.
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: "127.0.0.1:1", Weight: 1}},
	})

	hc := NewHealthChecker(bl)
	hc.Interval = 10 * time.Millisecond
	hc.Timeout = 50 * time.Millisecond
	hc.FailureThreshold = 2

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go hc.Run(ctx)

	deadline := time.After(400 * time.Millisecond)
	for {
		if bl.HealthyLen() == 0 {
			return // success
		}
		select {
		case <-deadline:
			t.Fatal("expected backend to be marked unhealthy within the deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHealthChecker_KeepsReachableBackendHealthy(t *testing.T) {
	addr, stop := listenOnce(t)
	defer stop()

	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: addr, Weight: 1}},
	})

	hc := NewHealthChecker(bl)
	hc.Interval = 10 * time.Millisecond
	hc.Timeout = 50 * time.Millisecond
	hc.SuccessThreshold = 1

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hc.Run(ctx)

	if bl.HealthyLen() != 1 {
		t.Errorf("expected reachable backend to remain healthy, got healthy count %d", bl.HealthyLen())
	}
}

func TestHealthChecker_RecoversAfterBackendComesBack(t *testing.T) {
	addr, stop := listenOnce(t)

	bl := NewBackendList()
	bl.Update(&pb.BackendSet{
		Group:    "test",
		Version:  1,
		Backends: []*pb.Backend{{Address: addr, Weight: 1}},
	})

	hc := NewHealthChecker(bl)
	hc.Interval = 10 * time.Millisecond
	hc.Timeout = 20 * time.Millisecond
	hc.FailureThreshold = 2
	hc.SuccessThreshold = 2

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go hc.Run(ctx)

	// Let it settle as healthy first.
	time.Sleep(50 * time.Millisecond)

	// Take the backend down and wait for it to be marked unhealthy.
	stop()
	deadlineUnhealthy := time.After(500 * time.Millisecond)
waitUnhealthy:
	for {
		if bl.HealthyLen() == 0 {
			break waitUnhealthy
		}
		select {
		case <-deadlineUnhealthy:
			t.Fatal("expected backend to be marked unhealthy after going down")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Bring an equivalent listener back up on the same address and expect
	// recovery once the success threshold is met.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("failed to re-listen on %s: %v", addr, err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	deadlineHealthy := time.After(1 * time.Second)
	for {
		if bl.HealthyLen() == 1 {
			return // success
		}
		select {
		case <-deadlineHealthy:
			t.Fatal("expected backend to recover to healthy after coming back up")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
