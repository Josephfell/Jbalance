// Package dataplane: tcpproxy.go implements the L4 (raw TCP) data plane
// mode — an alternative to Proxy's L7 HTTP reverse proxying, for
// protocols that aren't HTTP (databases, custom TCP protocols, TLS
// passthrough, etc). Selects a backend via the same BackendList used by
// the L7 proxy (same algorithms, same health checking, same control-plane
// push model) and pipes bytes bidirectionally between the client and
// whichever backend was selected.
//
// L4 mode has no application-layer visibility into the traffic it
// proxies, so L7-only features (host/path/method routing, sticky-session
// cookies) don't apply — an L4 listener is pinned to a single backend
// group for its lifetime, the same way an L7 data plane instance was
// before routing existed.
package dataplane

import (
	"context"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// TCPProxy accepts raw TCP connections and forwards each one to a backend
// selected from a single BackendList, for the lifetime of that
// connection — no per-request re-selection is possible at this layer,
// since a TCP proxy has no concept of "request" within a connection.
type TCPProxy struct {
	backends *BackendList
	// DialTimeout bounds how long connecting to the selected backend may
	// take before the client connection is closed. Defaults to 5s if
	// zero.
	DialTimeout time.Duration
}

// NewTCPProxy creates an L4 proxy that selects backends from backends.
func NewTCPProxy(backends *BackendList) *TCPProxy {
	return &TCPProxy{backends: backends}
}

func (p *TCPProxy) dialTimeout() time.Duration {
	if p.DialTimeout <= 0 {
		return 5 * time.Second
	}
	return p.DialTimeout
}

// Serve accepts connections from ln until ctx is cancelled or Accept
// returns a non-temporary error (including the listener being closed, as
// happens when the caller closes ln in response to ctx being cancelled —
// Serve itself never closes ln). Each accepted connection is handled in
// its own goroutine and does not block subsequent Accept calls.
func (p *TCPProxy) Serve(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down — the caller closed ln, this is expected
			}
			return err
		}
		go p.handleConn(ctx, conn)
	}
}

func (p *TCPProxy) handleConn(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close() }()

	addr, ok := p.backends.Next()
	if !ok {
		log.Printf("dataplane: tcp proxy: no healthy backends available, closing connection from %s", client.RemoteAddr())
		return
	}
	defer p.backends.Release(addr)

	dialCtx, cancel := context.WithTimeout(ctx, p.dialTimeout())
	upstream, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	cancel()
	if err != nil {
		log.Printf("dataplane: tcp proxy: failed to connect to backend %s: %v", addr, err)
		return
	}
	defer func() { _ = upstream.Close() }()

	pipeBidirectional(client, upstream)
}

// pipeBidirectional copies bytes in both directions between a and b until
// both directions have finished (each side's peer closed/finished
// writing), then returns. Uses CloseWrite (a half-close, sending TCP FIN
// or the TLS equivalent) rather than fully closing either connection when
// one direction finishes — some protocols rely on being able to keep
// reading a response after they've finished sending their request, and a
// full Close here would cut that off. The caller closes both connections
// fully once pipeBidirectional returns.
func pipeBidirectional(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()

	wg.Wait()
}

// halfCloser is implemented by *net.TCPConn and *tls.Conn (among others)
// — narrower than net.Conn so pipeBidirectional can half-close a
// connection without assuming a concrete type, working the same whether
// or not the client side is TLS-terminated.
type halfCloser interface {
	CloseWrite() error
}

func closeWrite(conn net.Conn) {
	if hc, ok := conn.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}
