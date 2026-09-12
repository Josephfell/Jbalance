package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCertKey writes a throwaway self-signed cert + key pair into dir
// and returns their paths. Used to exercise the cert-file existence and
// load checks without shelling out to openssl.
func writeTestCertKey(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"test.local"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = certOut.Close()

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = keyOut.Close()
	return certPath, keyPath
}

// validDataPlane returns a baseline data-plane config that passes
// validation, so each test case can flip exactly one thing.
func validDataPlane() DataPlaneConfig {
	return DataPlaneConfig{
		Protocol:                "http",
		ListenAddr:              ":8080",
		ControlPlaneAddr:        "localhost:9090",
		Group:                   "web-tier",
		MetricsAddr:             ":9100",
		MetricsEnabled:          true,
		OpsAddr:                 ":9101",
		HealthCheckMode:         "tcp",
		HealthCheckScheme:       "http",
		HealthCheckInterval:     5 * time.Second,
		HealthCheckTimeout:      2 * time.Second,
		HealthCheckExpectStatus: 0,
		UnhealthyThreshold:      3,
		HealthyThreshold:        2,
		BackendProtocol:         "http1",
		AccessLogFormat:         "json",
		ProxyConnectTimeout:     5 * time.Second,
		ProxyResponseTimeout:    30 * time.Second,
		ProxyMaxRetries:         1,
		ProxyRetryBackoff:       50 * time.Millisecond,
		TCPDialTimeout:          5 * time.Second,
		ShutdownGrace:           5 * time.Second,
		OutlierConsecutiveError: 5,
		OutlierEjectDuration:    30 * time.Second,
		OutlierMaxEjectDuration: 5 * time.Minute,
		OutlierMaxEjectPercent:  50,
		LogLevel:                "info",
		LogFormat:               "text",
	}
}

func TestValidateDataPlane(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*DataPlaneConfig)
		wantErr bool
		// substr, if non-empty, must appear in the error message.
		substr string
	}{
		{name: "valid baseline", mutate: func(*DataPlaneConfig) {}, wantErr: false},
		{name: "bad protocol", mutate: func(c *DataPlaneConfig) { c.Protocol = "udp" }, wantErr: true, substr: "-protocol"},
		{name: "bad health-check-mode", mutate: func(c *DataPlaneConfig) { c.HealthCheckMode = "ping" }, wantErr: true, substr: "-health-check-mode"},
		{name: "bad health-check-scheme", mutate: func(c *DataPlaneConfig) { c.HealthCheckScheme = "ftp" }, wantErr: true, substr: "-health-check-scheme"},
		{name: "bad backend-protocol", mutate: func(c *DataPlaneConfig) { c.BackendProtocol = "http3" }, wantErr: true, substr: "-backend-protocol"},
		{name: "bad access-log-format", mutate: func(c *DataPlaneConfig) { c.AccessLogFormat = "xml" }, wantErr: true, substr: "-access-log-format"},
		{name: "bad log-level", mutate: func(c *DataPlaneConfig) { c.LogLevel = "trace" }, wantErr: true, substr: "-log-level"},
		{name: "bad log-format", mutate: func(c *DataPlaneConfig) { c.LogFormat = "yaml" }, wantErr: true, substr: "-log-format"},
		{name: "empty group", mutate: func(c *DataPlaneConfig) { c.Group = "  " }, wantErr: true, substr: "-group"},
		{name: "empty listen-addr", mutate: func(c *DataPlaneConfig) { c.ListenAddr = "" }, wantErr: true, substr: "-listen-addr"},
		{name: "listen-addr no port", mutate: func(c *DataPlaneConfig) { c.ListenAddr = "localhost" }, wantErr: true, substr: "-listen-addr"},
		{name: "listen-addr port 0", mutate: func(c *DataPlaneConfig) { c.ListenAddr = ":0" }, wantErr: true, substr: "port 0"},
		{name: "empty control-plane-addr", mutate: func(c *DataPlaneConfig) { c.ControlPlaneAddr = "" }, wantErr: true, substr: "-control-plane-addr"},
		{name: "control-plane-addr no host", mutate: func(c *DataPlaneConfig) { c.ControlPlaneAddr = ":9090" }, wantErr: true, substr: "missing a host"},
		{name: "ops-addr empty is allowed", mutate: func(c *DataPlaneConfig) { c.OpsAddr = "" }, wantErr: false},
		{name: "negative health interval", mutate: func(c *DataPlaneConfig) { c.HealthCheckInterval = -1 }, wantErr: true, substr: "-health-check-interval"},
		{name: "zero health timeout", mutate: func(c *DataPlaneConfig) { c.HealthCheckTimeout = 0 }, wantErr: true, substr: "-health-check-timeout"},
		{name: "zero unhealthy threshold", mutate: func(c *DataPlaneConfig) { c.UnhealthyThreshold = 0 }, wantErr: true, substr: "-unhealthy-threshold"},
		{name: "bad expect-status", mutate: func(c *DataPlaneConfig) { c.HealthCheckExpectStatus = 42 }, wantErr: true, substr: "-health-check-expect-status"},
		{name: "expect-status 0 ok", mutate: func(c *DataPlaneConfig) { c.HealthCheckExpectStatus = 0 }, wantErr: false},
		{name: "expect-status 200 ok", mutate: func(c *DataPlaneConfig) { c.HealthCheckExpectStatus = 200 }, wantErr: false},
		{name: "zero connect timeout", mutate: func(c *DataPlaneConfig) { c.ProxyConnectTimeout = 0 }, wantErr: true, substr: "-proxy-connect-timeout"},
		{name: "response timeout 0 ok (disabled)", mutate: func(c *DataPlaneConfig) { c.ProxyResponseTimeout = 0 }, wantErr: false},
		{name: "negative response timeout", mutate: func(c *DataPlaneConfig) { c.ProxyResponseTimeout = -1 }, wantErr: true, substr: "-proxy-response-timeout"},
		{name: "negative max retries", mutate: func(c *DataPlaneConfig) { c.ProxyMaxRetries = -1 }, wantErr: true, substr: "-proxy-max-retries"},
		{name: "max retries 0 ok", mutate: func(c *DataPlaneConfig) { c.ProxyMaxRetries = 0 }, wantErr: false},
		{name: "zero tcp dial timeout", mutate: func(c *DataPlaneConfig) { c.TCPDialTimeout = 0 }, wantErr: true, substr: "-tcp-dial-timeout"},
		{name: "negative shutdown grace", mutate: func(c *DataPlaneConfig) { c.ShutdownGrace = -1 }, wantErr: true, substr: "-shutdown-grace"},
		{name: "shutdown grace 0 ok", mutate: func(c *DataPlaneConfig) { c.ShutdownGrace = 0 }, wantErr: false},
		{
			name: "outlier bad consecutive errors",
			mutate: func(c *DataPlaneConfig) {
				c.OutlierDetection = true
				c.OutlierConsecutiveError = 0
			},
			wantErr: true, substr: "-outlier-consecutive-errors",
		},
		{
			name: "outlier max < base eject",
			mutate: func(c *DataPlaneConfig) {
				c.OutlierDetection = true
				c.OutlierEjectDuration = time.Minute
				c.OutlierMaxEjectDuration = time.Second
			},
			wantErr: true, substr: "must be >=",
		},
		{
			name: "outlier percent out of range",
			mutate: func(c *DataPlaneConfig) {
				c.OutlierDetection = true
				c.OutlierMaxEjectPercent = 150
			},
			wantErr: true, substr: "-outlier-max-eject-percent",
		},
		{
			name: "outlier fields ignored when disabled",
			mutate: func(c *DataPlaneConfig) {
				c.OutlierDetection = false
				c.OutlierConsecutiveError = 0
				c.OutlierMaxEjectPercent = 999
			},
			wantErr: false,
		},
		{name: "cp client cert without key", mutate: func(c *DataPlaneConfig) { c.CPTLSEnable = true; c.CPTLSClientCert = "/x/c.pem" }, wantErr: true, substr: "must be set together"},
		{name: "cp tls material without enable", mutate: func(c *DataPlaneConfig) { c.CPTLSCACert = "/x/ca.pem" }, wantErr: true, substr: "no effect without -control-plane-tls"},
		{name: "http tls cert without key", mutate: func(c *DataPlaneConfig) { c.HTTPTLSCert = "/x/c.pem" }, wantErr: true, substr: "-http-tls-cert and -http-tls-key must be set together"},
		{name: "http client-ca without server cert", mutate: func(c *DataPlaneConfig) { c.HTTPTLSClientCA = "/x/ca.pem" }, wantErr: true, substr: "requires a server certificate"},
		{name: "missing tls cert file", mutate: func(c *DataPlaneConfig) { c.HTTPTLSCert = "/no/such/cert.pem"; c.HTTPTLSKey = "/no/such/key.pem" }, wantErr: true, substr: "cannot be accessed"},
		{name: "malformed sni certs", mutate: func(c *DataPlaneConfig) { c.HTTPTLSCerts = "not-a-pair" }, wantErr: true, substr: "malformed"},
		{name: "negative reload interval", mutate: func(c *DataPlaneConfig) { c.HTTPTLSReloadInterval = -1 }, wantErr: true, substr: "-http-tls-reload-interval"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validDataPlane()
			tt.mutate(&c)
			err := ValidateDataPlane(c)
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tt.substr != "" && (err == nil || !strings.Contains(err.Error(), tt.substr)) {
				t.Fatalf("error %v does not contain %q", err, tt.substr)
			}
		})
	}
}

// TestValidateDataPlaneRealCertsLoad confirms a genuinely valid cert pair
// passes the load check (the happy path of the fail-fast cert loading).
func TestValidateDataPlaneRealCertsLoad(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTestCertKey(t, dir)
	c := validDataPlane()
	c.HTTPTLSCert = cert
	c.HTTPTLSKey = key
	if err := ValidateDataPlane(c); err != nil {
		t.Fatalf("valid cert pair should pass, got: %v", err)
	}
}

// TestValidateDataPlaneReportsAllProblems confirms the validator does not
// stop at the first error — a config with several problems lists them all.
func TestValidateDataPlaneReportsAllProblems(t *testing.T) {
	c := validDataPlane()
	c.Protocol = "udp"
	c.LogLevel = "trace"
	c.HealthCheckTimeout = 0
	err := ValidateDataPlane(c)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"-protocol", "-log-level", "-health-check-timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("combined error missing %q; got: %v", want, err)
		}
	}
}

func validControlPlane() ControlPlaneConfig {
	return ControlPlaneConfig{
		GRPCAddr:          ":9090",
		AdminAddr:         ":9091",
		ReconcileInterval: 2 * time.Second,
		Provider:          "fake",
		FakeBasePort:      8081,
		ScalingInterval:   5 * time.Second,
		SimulateScaling:   true,
		LogLevel:          "info",
		LogFormat:         "text",
	}
}

func TestValidateControlPlane(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ControlPlaneConfig)
		wantErr bool
		substr  string
	}{
		{name: "valid baseline", mutate: func(*ControlPlaneConfig) {}, wantErr: false},
		{name: "bad provider", mutate: func(c *ControlPlaneConfig) { c.Provider = "gcp" }, wantErr: true, substr: "-provider"},
		{name: "bad log-level", mutate: func(c *ControlPlaneConfig) { c.LogLevel = "silly" }, wantErr: true, substr: "-log-level"},
		{name: "empty grpc-addr", mutate: func(c *ControlPlaneConfig) { c.GRPCAddr = "" }, wantErr: true, substr: "-grpc-addr"},
		{name: "grpc-addr no port", mutate: func(c *ControlPlaneConfig) { c.GRPCAddr = "host" }, wantErr: true, substr: "-grpc-addr"},
		{name: "empty admin-addr when enabled", mutate: func(c *ControlPlaneConfig) { c.AdminAddr = "" }, wantErr: true, substr: "-admin-addr"},
		{name: "empty admin-addr ok when disabled", mutate: func(c *ControlPlaneConfig) { c.AdminAddr = ""; c.AdminDisable = true }, wantErr: false},
		{name: "zero reconcile interval", mutate: func(c *ControlPlaneConfig) { c.ReconcileInterval = 0 }, wantErr: true, substr: "-reconcile-interval"},
		{name: "fake base port out of range", mutate: func(c *ControlPlaneConfig) { c.FakeBasePort = 70000 }, wantErr: true, substr: "-fake-base-port"},
		{name: "fake zero scaling interval when simulating", mutate: func(c *ControlPlaneConfig) { c.ScalingInterval = 0 }, wantErr: true, substr: "-scaling-interval"},
		{name: "fake zero scaling interval ok when not simulating", mutate: func(c *ControlPlaneConfig) { c.ScalingInterval = 0; c.SimulateScaling = false }, wantErr: false},
		{name: "tls cert without key", mutate: func(c *ControlPlaneConfig) { c.TLSCert = "/x/c.pem" }, wantErr: true, substr: "-tls-cert and -tls-key must be set together"},
		{name: "client-ca without server cert", mutate: func(c *ControlPlaneConfig) { c.TLSClientCA = "/x/ca.pem" }, wantErr: true, substr: "-tls-client-ca requires"},
		{name: "missing tls cert file", mutate: func(c *ControlPlaneConfig) { c.TLSCert = "/no/c.pem"; c.TLSKey = "/no/k.pem" }, wantErr: true, substr: "cannot be accessed"},
		{name: "admin tls cert without key", mutate: func(c *ControlPlaneConfig) { c.AdminTLSCert = "/x/c.pem" }, wantErr: true, substr: "-admin-tls-cert and -admin-tls-key"},
		{
			name: "azure requires subscription/rg/groups",
			mutate: func(c *ControlPlaneConfig) {
				c.Provider = "azure-vmss"
			},
			wantErr: true, substr: "azure-vmss",
		},
		{
			name: "azure valid",
			mutate: func(c *ControlPlaneConfig) {
				c.Provider = "azure-vmss"
				c.AzureSubscriptionID = "sub"
				c.AzureResourceGroup = "rg"
				c.AzureVMSSGroups = "web:vmss:8080"
			},
			wantErr: false,
		},
		{
			name: "kubernetes requires groups",
			mutate: func(c *ControlPlaneConfig) {
				c.Provider = "kubernetes"
			},
			wantErr: true, substr: "-k8s-groups",
		},
		{
			name: "kubernetes valid",
			mutate: func(c *ControlPlaneConfig) {
				c.Provider = "kubernetes"
				c.K8sGroups = "web:default:web:8080"
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validControlPlane()
			tt.mutate(&c)
			err := ValidateControlPlane(c)
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if tt.substr != "" && (err == nil || !strings.Contains(err.Error(), tt.substr)) {
				t.Fatalf("error %v does not contain %q", err, tt.substr)
			}
		})
	}
}

func TestValidateControlPlaneRealCertsLoad(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeTestCertKey(t, dir)
	c := validControlPlane()
	c.TLSCert = cert
	c.TLSKey = key
	if err := ValidateControlPlane(c); err != nil {
		t.Fatalf("valid cert pair should pass, got: %v", err)
	}
}
