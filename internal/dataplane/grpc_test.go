package dataplane

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"

	pb "github.com/Josephfell/Jbalance/proto"
)

// startH2CServer starts an h2c (cleartext HTTP/2) server on a random port
// running handler, and returns its "host:port" address plus a cleanup.
func startH2CServer(t *testing.T, handler http.Handler) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: &protos}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// h2cClient returns an http.Client that speaks prior-knowledge HTTP/2 over
// cleartext (no TLS), the same way a gRPC client without TLS does.
func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

func TestProxy_H2C_EndToEnd(t *testing.T) {
	// Backend that only speaks HTTP/2 (h2c) and echoes back the protocol
	// major/minor it saw — proving the request reached it over HTTP/2, not
	// downgraded to 1.1.
	backendAddr, stopBackend := startH2CServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.ProtoAtLeast(2, 0) {
			t.Errorf("backend received proto %s, want HTTP/2", r.Proto)
		}
		fmt.Fprintf(w, "backend-proto=%s path=%s", r.Proto, r.URL.Path)
	}))
	defer stopBackend()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routes := NewRouteTable("web-tier")
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp-test", nil,
		HealthCheckConfig{Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 2}, time.Hour)
	groups.Ensure("web-tier").Update(&pb.BackendSet{
		Group: "web-tier", Version: 1,
		Backends: []*pb.Backend{{Address: backendAddr, Weight: 1}},
	})

	proxy := NewProxy(routes, groups, nil, ProxyConfig{BackendProtocol: "h2c"})

	// Run the proxy on a real listener, itself serving h2c so an HTTP/2
	// client can connect.
	proxyAddr, stopProxy := startH2CServer(t, proxy.Handler())
	defer stopProxy()

	client := h2cClient()
	resp, err := client.Get("http://" + proxyAddr + "/grpc.Service/Method")
	if err != nil {
		t.Fatalf("request through proxy failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ProtoMajor != 2 {
		t.Errorf("client saw response proto %s from proxy, want HTTP/2", resp.Proto)
	}
	body, _ := io.ReadAll(resp.Body)
	want := "backend-proto=HTTP/2.0 path=/grpc.Service/Method"
	if string(body) != want {
		t.Errorf("body = %q, want %q", string(body), want)
	}
}

func TestProxy_H2C_ConfigSelectsTransport(t *testing.T) {
	// A sanity check that the default (http1) path still builds a working
	// proxy and an h2c-configured proxy is distinct — construction only.
	routes := NewRouteTable("g")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	groups := NewGroupManager(ctx, "127.0.0.1:1", "dp", nil,
		HealthCheckConfig{Interval: time.Hour, Timeout: time.Second}, time.Hour)

	if p := NewProxy(routes, groups, nil, ProxyConfig{}); p.rp.Transport == nil {
		t.Error("http1 proxy has nil transport")
	}
	if p := NewProxy(routes, groups, nil, ProxyConfig{BackendProtocol: "h2c"}); p.rp.Transport == nil {
		t.Error("h2c proxy has nil transport")
	}
}
