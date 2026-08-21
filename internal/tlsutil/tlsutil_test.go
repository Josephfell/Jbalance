package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateSelfSignedCert creates a throwaway self-signed cert/key pair for
// testing, written to PEM files in dir, and returns their paths.
func generateSelfSignedCert(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("failed to create cert file: %v", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		t.Fatalf("failed to write cert PEM: %v", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("failed to create key file: %v", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("failed to write key PEM: %v", err)
	}

	return certPath, keyPath
}

func TestLoadServerConfig_Success(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "server")

	cfg, err := LoadServerConfig(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("LoadServerConfig() error: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected 1 certificate loaded, got %d", len(cfg.Certificates))
	}
	if cfg.ClientAuth != 0 {
		t.Errorf("expected no client auth requirement when caCertFile is empty, got %v", cfg.ClientAuth)
	}
}

func TestLoadServerConfig_MissingFile(t *testing.T) {
	if _, err := LoadServerConfig("/does/not/exist.crt", "/does/not/exist.key", ""); err == nil {
		t.Error("expected an error for a missing cert/key file, got nil")
	}
}

func TestLoadServerConfig_MutualTLSRequiresClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "server")
	caCertPath, _ := generateSelfSignedCert(t, dir, "ca")

	cfg, err := LoadServerConfig(certPath, keyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadServerConfig() error: %v", err)
	}
	if cfg.ClientAuth == 0 {
		t.Error("expected client auth to be required when caCertFile is set")
	}
	if cfg.ClientCAs == nil {
		t.Error("expected ClientCAs pool to be populated")
	}
}

func TestLoadClientConfig_WithSystemRoots(t *testing.T) {
	cfg, err := LoadClientConfig("", "", "")
	if err != nil {
		t.Fatalf("LoadClientConfig() error: %v", err)
	}
	if cfg.RootCAs != nil {
		t.Error("expected RootCAs to be nil (system default) when caCertFile is empty")
	}
	if len(cfg.Certificates) != 0 {
		t.Error("expected no client certificates when certFile/keyFile are empty")
	}
}

func TestLoadClientConfig_WithClientCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := generateSelfSignedCert(t, dir, "client")

	cfg, err := LoadClientConfig(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("LoadClientConfig() error: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected 1 client certificate loaded, got %d", len(cfg.Certificates))
	}
}

func TestLoadClientConfig_InvalidCAFile(t *testing.T) {
	dir := t.TempDir()
	badCAPath := filepath.Join(dir, "bad-ca.crt")
	if err := os.WriteFile(badCAPath, []byte("not a real cert"), 0o600); err != nil {
		t.Fatalf("failed to write bad CA file: %v", err)
	}

	if _, err := LoadClientConfig("", "", badCAPath); err == nil {
		t.Error("expected an error for an invalid CA cert file, got nil")
	}
}
