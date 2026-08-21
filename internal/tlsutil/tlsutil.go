// Package tlsutil provides small helpers for loading TLS configuration
// from cert/key file paths, shared by the control plane's gRPC server and
// the data plane's gRPC client and HTTP listener. TLS is optional
// everywhere it's used — when no cert/key is configured, callers fall back
// to plaintext, which is fine for local development but should not be used
// across a real network.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// LoadServerConfig builds a *tls.Config for a server from a cert/key pair
// on disk. If caCertFile is non-empty, client certificates will be
// required and verified against it (mutual TLS) — leave empty for
// server-only TLS.
func LoadServerConfig(certFile, keyFile, caCertFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: failed to load server cert/key: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if caCertFile != "" {
		pool, err := loadCertPool(caCertFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return cfg, nil
}

// LoadClientConfig builds a *tls.Config for a client connecting to a TLS
// server. If caCertFile is empty, the system root CA pool is used (for
// connecting to a server with a certificate from a public CA). If
// certFile/keyFile are provided, the client presents them (mutual TLS).
func LoadClientConfig(certFile, keyFile, caCertFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if caCertFile != "" {
		pool, err := loadCertPool(caCertFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}

	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: failed to load client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

func loadCertPool(caCertFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: failed to read CA cert %s: %w", caCertFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tlsutil: failed to parse CA cert %s", caCertFile)
	}
	return pool, nil
}
