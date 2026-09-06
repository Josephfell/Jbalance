package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genCertForNames writes a self-signed cert/key with the given DNS SANs to
// dir and returns their paths.
func genCertForNames(t *testing.T, dir, name string, dnsNames ...string) (certPath, keyPath string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, keyPath, "EC PRIVATE KEY", keyBytes)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

func TestParseCertPairs(t *testing.T) {
	pairs, err := ParseCertPairs("web.crt:web.key, api.crt:api.key")
	if err != nil {
		t.Fatalf("ParseCertPairs error: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(pairs))
	}
	if pairs[0].CertFile != "web.crt" || pairs[0].KeyFile != "web.key" {
		t.Errorf("pair 0 = %+v", pairs[0])
	}
	if pairs[1].CertFile != "api.crt" || pairs[1].KeyFile != "api.key" {
		t.Errorf("pair 1 = %+v", pairs[1])
	}

	if _, err := ParseCertPairs(""); err != nil {
		t.Errorf("empty spec should be nil,nil; got err %v", err)
	}
	if _, err := ParseCertPairs("no-colon-here"); err == nil {
		t.Error("expected error for a pair with no colon")
	}
}

func TestCertReloader_SNISelectsMatchingCert(t *testing.T) {
	dir := t.TempDir()
	webCert, webKey := genCertForNames(t, dir, "web", "web.example.com")
	apiCert, apiKey := genCertForNames(t, dir, "api", "api.example.com")

	r, err := NewCertReloader([]CertPair{
		{CertFile: webCert, KeyFile: webKey},
		{CertFile: apiCert, KeyFile: apiKey},
	})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// SNI for api.example.com must pick the api cert.
	got, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got.Leaf.Subject.CommonName != "api.example.com" {
		t.Errorf("SNI api.example.com selected cert CN=%q, want api.example.com", got.Leaf.Subject.CommonName)
	}

	// SNI for web.example.com must pick the web cert.
	got, err = r.GetCertificate(&tls.ClientHelloInfo{ServerName: "web.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got.Leaf.Subject.CommonName != "web.example.com" {
		t.Errorf("SNI web.example.com selected cert CN=%q, want web.example.com", got.Leaf.Subject.CommonName)
	}
}

func TestCertReloader_NoSNIFallsBackToFirst(t *testing.T) {
	dir := t.TempDir()
	webCert, webKey := genCertForNames(t, dir, "web", "web.example.com")
	apiCert, apiKey := genCertForNames(t, dir, "api", "api.example.com")
	r, err := NewCertReloader([]CertPair{
		{CertFile: webCert, KeyFile: webKey},
		{CertFile: apiCert, KeyFile: apiKey},
	})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// No SNI (raw-IP client): first cert.
	got, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: ""})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got.Leaf.Subject.CommonName != "web.example.com" {
		t.Errorf("no-SNI fallback CN=%q, want the first cert web.example.com", got.Leaf.Subject.CommonName)
	}

	// Unknown SNI: also first cert.
	got, _ = r.GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.example.org"})
	if got.Leaf.Subject.CommonName != "web.example.com" {
		t.Errorf("unknown-SNI fallback CN=%q, want web.example.com", got.Leaf.Subject.CommonName)
	}
}

func TestCertReloader_ReloadPicksUpNewCert(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genCertForNames(t, dir, "svc", "old.example.com")
	r, err := NewCertReloader([]CertPair{{CertFile: certPath, KeyFile: keyPath}})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	got, _ := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "old.example.com"})
	if got.Leaf.Subject.CommonName != "old.example.com" {
		t.Fatalf("initial CN=%q, want old.example.com", got.Leaf.Subject.CommonName)
	}

	// Overwrite the same paths with a cert for a new name, then reload.
	genCertForNames(t, dir, "svc", "new.example.com")
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got, _ = r.GetCertificate(&tls.ClientHelloInfo{ServerName: "new.example.com"})
	if got.Leaf.Subject.CommonName != "new.example.com" {
		t.Errorf("after reload CN=%q, want new.example.com", got.Leaf.Subject.CommonName)
	}
}

func TestCertReloader_BadReloadKeepsPrevious(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := genCertForNames(t, dir, "svc", "good.example.com")
	r, err := NewCertReloader([]CertPair{{CertFile: certPath, KeyFile: keyPath}})
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// Corrupt the cert file, then attempt a reload.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	if err := r.Reload(); err == nil {
		t.Fatal("expected Reload to fail on a corrupt cert file")
	}
	// The previously-good cert must still be served.
	got, err := r.GetCertificate(&tls.ClientHelloInfo{ServerName: "good.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate after bad reload: %v", err)
	}
	if got.Leaf.Subject.CommonName != "good.example.com" {
		t.Errorf("after bad reload CN=%q, want the retained good.example.com", got.Leaf.Subject.CommonName)
	}
}

func TestNewCertReloader_RequiresAtLeastOnePair(t *testing.T) {
	if _, err := NewCertReloader(nil); err == nil {
		t.Error("expected error for zero cert pairs")
	}
}
