package dataplane

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

// newEchoBackend starts a real TCP listener that echoes every line it
// receives back to the client, prefixed with label — enough to prove
// which backend a connection actually reached and that bytes flow in
// both directions.
func newEchoBackend(t *testing.T, label string) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo backend: %v", err)
	}

	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					if _, err := c.Write([]byte(label + ":" + scanner.Text() + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
	}
}

func dialAndExchange(t *testing.T, addr, message string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial proxy at %s: %v", addr, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message + "\n")); err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("failed to read response from proxy: %v", scanner.Err())
	}
	return scanner.Text()
}

func startTCPProxyListener(t *testing.T, proxy *TCPProxy, ctx context.Context) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy listener: %v", err)
	}
	go func() {
		_ = proxy.Serve(ctx, ln)
	}()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return ln.Addr().String()
}

func TestTCPProxy_ForwardsToBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backendAddr, cleanup := newEchoBackend(t, "echo")
	defer cleanup()

	backends := NewBackendList()
	backends.Update(&pb.BackendSet{Group: "tcp-tier", Version: 1, Backends: []*pb.Backend{{Address: backendAddr, Weight: 1}}})

	proxyAddr := startTCPProxyListener(t, NewTCPProxy(backends), ctx)

	got := dialAndExchange(t, proxyAddr, "hello")
	if got != "echo:hello" {
		t.Errorf("expected the proxy to forward to the backend and relay its response, got %q", got)
	}
}

func TestTCPProxy_DistributesAcrossBackends(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrA, cleanupA := newEchoBackend(t, "a")
	defer cleanupA()
	addrB, cleanupB := newEchoBackend(t, "b")
	defer cleanupB()

	backends := NewBackendList()
	backends.Update(&pb.BackendSet{
		Group: "tcp-tier", Version: 1,
		Backends: []*pb.Backend{{Address: addrA, Weight: 1}, {Address: addrB, Weight: 1}},
	})

	proxyAddr := startTCPProxyListener(t, NewTCPProxy(backends), ctx)

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got := dialAndExchange(t, proxyAddr, "ping")
		switch got {
		case "a:ping":
			seen["a"] = true
		case "b:ping":
			seen["b"] = true
		default:
			t.Fatalf("unexpected response: %q", got)
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("expected round-robin to reach both backends across 4 connections, got %v", seen)
	}
}

func TestTCPProxy_NoHealthyBackendsClosesConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backends := NewBackendList() // never updated — no backends at all

	proxyAddr := startTCPProxyListener(t, NewTCPProxy(backends), ctx)

	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to dial proxy: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected the connection to be closed when no healthy backends are available")
	}
}

func TestTCPProxy_SkipsUnhealthyBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrA, cleanupA := newEchoBackend(t, "a")
	defer cleanupA()
	addrB, cleanupB := newEchoBackend(t, "b")
	defer cleanupB()

	backends := NewBackendList()
	backends.Update(&pb.BackendSet{
		Group: "tcp-tier", Version: 1,
		Backends: []*pb.Backend{{Address: addrA, Weight: 1}, {Address: addrB, Weight: 1}},
	})
	backends.SetHealth(addrA, false)

	proxyAddr := startTCPProxyListener(t, NewTCPProxy(backends), ctx)

	for i := 0; i < 3; i++ {
		got := dialAndExchange(t, proxyAddr, "ping")
		if got != "b:ping" {
			t.Errorf("expected every connection to reach the only healthy backend (b), got %q", got)
		}
	}
}

func TestTCPProxy_DialTimeoutDefault(t *testing.T) {
	p := NewTCPProxy(NewBackendList())
	if p.dialTimeout() != 5*time.Second {
		t.Errorf("expected default dial timeout of 5s, got %v", p.dialTimeout())
	}
	p.DialTimeout = 500 * time.Millisecond
	if p.dialTimeout() != 500*time.Millisecond {
		t.Errorf("expected configured dial timeout to take effect, got %v", p.dialTimeout())
	}
}
