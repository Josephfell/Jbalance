package dataplane

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/Josephfell/Jbalance/proto"
)

func TestRequestIsUpgrade(t *testing.T) {
	tests := []struct {
		conn, upgrade string
		want          bool
	}{
		{"Upgrade", "websocket", true},
		{"keep-alive, Upgrade", "websocket", true},
		{"upgrade", "websocket", true},
		{"Upgrade", "", false}, // Connection: Upgrade but no Upgrade header
		{"keep-alive", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "http://x/", nil)
		if tt.conn != "" {
			r.Header.Set("Connection", tt.conn)
		}
		if tt.upgrade != "" {
			r.Header.Set("Upgrade", tt.upgrade)
		}
		if got := requestIsUpgrade(r); got != tt.want {
			t.Errorf("conn=%q upgrade=%q: requestIsUpgrade=%v want %v", tt.conn, tt.upgrade, got, tt.want)
		}
	}
}

func TestUpgradeRequestNotRetryable(t *testing.T) {
	// A bodyless GET is normally retryable, but an upgrade GET must not be
	// (it can't be buffered/replayed).
	r := httptest.NewRequest("GET", "http://x/", nil)
	if !requestRetryable(r) {
		t.Fatal("plain GET should be retryable")
	}
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	if requestRetryable(r) {
		t.Fatal("upgrade GET must NOT be retryable")
	}
}

// TestProxyWebSocketUpgrade proxies a raw HTTP Upgrade handshake through
// the L7 proxy to a backend that hijacks the connection and echoes a line,
// proving bidirectional bytes flow across the proxy for an upgraded conn.
func TestProxyWebSocketUpgrade(t *testing.T) {
	// Backend: complete the upgrade handshake, then echo one line.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "echo") {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = buf.Flush()
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer backend.Close()
	backendAddr := strings.TrimPrefix(backend.URL, "http://")

	// Proxy in front of a single-backend group.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil, "", HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	bl := groups.Ensure("web-tier")
	bl.Update(&pb.BackendSet{Group: "web-tier", Version: 1, Backends: []*pb.Backend{{Address: backendAddr, Weight: 1}}})
	bl.SetHealth(backendAddr, true)
	routes := NewRouteTable("web-tier")

	proxy := httptest.NewServer(NewProxy(routes, groups, nil, ProxyConfig{}).Handler())
	defer proxy.Close()
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	// Raw client: open a TCP conn to the proxy and send an Upgrade request.
	conn, err := net.DialTimeout("tcp", proxyAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", proxyAddr)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write req: %v", err)
	}
	br := bufio.NewReader(conn)
	// Read the 101 status line.
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("expected 101 Switching Protocols, got %q", status)
	}
	// Drain remaining handshake headers up to the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	// Now the connection is upgraded: send a line, expect it echoed.
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	echoed, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if strings.TrimSpace(echoed) != "echo:ping" {
		t.Fatalf("echo = %q, want %q", strings.TrimSpace(echoed), "echo:ping")
	}
}
