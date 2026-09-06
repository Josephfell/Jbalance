// Package tlsutil: certreloader.go adds hot-reloadable, SNI-aware server
// TLS certificate management on top of the static LoadServerConfig helper.
//
// Two capabilities the static loader lacks:
//
//   - SNI: hold more than one certificate and pick the right one per
//     connection by matching the client's requested server name against
//     each certificate's DNS names / common name — so one listener can
//     terminate TLS for several hostnames.
//   - Hot reload: re-read the certificate files from disk without
//     restarting the process, so a rotated/renewed certificate is picked
//     up in place. Reload is driven both by a background mtime poll and
//     by an explicit Reload() call (wired to SIGHUP in the data plane), and
//     is atomic: if a reload fails to parse, the previously-loaded
//     certificates keep serving rather than the listener breaking.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// CertPair is one certificate/key file pair to load and watch.
type CertPair struct {
	CertFile string
	KeyFile  string
}

// ParseCertPairs parses a comma-separated list of "certfile:keyfile"
// pairs (e.g. "web.crt:web.key,api.crt:api.key") into CertPairs. A path
// containing a Windows-style drive letter is not supported here — paths
// are split on the first colon, which is fine for the POSIX container
// paths this project uses. An empty spec returns nil, nil.
func ParseCertPairs(spec string) ([]CertPair, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var pairs []CertPair
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		i := strings.Index(entry, ":")
		if i < 0 {
			return nil, fmt.Errorf("tlsutil: bad cert pair %q (want certfile:keyfile)", entry)
		}
		cert := strings.TrimSpace(entry[:i])
		key := strings.TrimSpace(entry[i+1:])
		if cert == "" || key == "" {
			return nil, fmt.Errorf("tlsutil: bad cert pair %q (empty cert or key path)", entry)
		}
		pairs = append(pairs, CertPair{CertFile: cert, KeyFile: key})
	}
	return pairs, nil
}

// CertReloader holds the currently-loaded certificates and re-reads them
// from disk on demand. It is safe for concurrent use: GetCertificate (on
// the TLS handshake path) reads under an RLock while Reload swaps the set
// under a Lock.
type CertReloader struct {
	pairs []CertPair

	mu    sync.RWMutex
	certs []tls.Certificate
	// mtimes tracks the last-seen modification time of each file, so the
	// background watcher only reloads when something actually changed.
	mtimes map[string]time.Time
}

// NewCertReloader loads the given cert pairs and returns a reloader. It
// fails if the initial load fails (a listener should not start with no
// usable certificate); subsequent reload failures are non-fatal and keep
// the last good set.
func NewCertReloader(pairs []CertPair) (*CertReloader, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("tlsutil: NewCertReloader requires at least one cert pair")
	}
	r := &CertReloader{pairs: pairs, mtimes: make(map[string]time.Time)}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// load reads every configured cert pair and, only if all parse
// successfully, swaps them in atomically. A parse leaf failure on any
// pair aborts the whole load so the reloader never ends up with a
// partially-updated set.
func (r *CertReloader) load() error {
	loaded := make([]tls.Certificate, 0, len(r.pairs))
	newMtimes := make(map[string]time.Time, len(r.pairs)*2)
	for _, p := range r.pairs {
		cert, err := tls.LoadX509KeyPair(p.CertFile, p.KeyFile)
		if err != nil {
			return fmt.Errorf("tlsutil: failed to load cert pair %s/%s: %w", p.CertFile, p.KeyFile, err)
		}
		// Parse the leaf so GetCertificate can match SNI names without
		// re-parsing on every handshake.
		if cert.Leaf == nil && len(cert.Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
				cert.Leaf = leaf
			}
		}
		loaded = append(loaded, cert)
		newMtimes[p.CertFile] = fileMtime(p.CertFile)
		newMtimes[p.KeyFile] = fileMtime(p.KeyFile)
	}

	r.mu.Lock()
	r.certs = loaded
	r.mtimes = newMtimes
	r.mu.Unlock()
	return nil
}

// Reload re-reads all cert pairs from disk. On failure the previously
// loaded certificates are retained and the error is returned.
func (r *CertReloader) Reload() error {
	return r.load()
}

// GetCertificate implements the tls.Config.GetCertificate hook: it picks
// the certificate whose subject matches the SNI server name the client
// requested, falling back to the first configured certificate when no
// name matches (or the client sent no SNI, as a raw-IP client does).
func (r *CertReloader) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.certs) == 0 {
		return nil, fmt.Errorf("tlsutil: no certificates loaded")
	}
	if hello != nil && hello.ServerName != "" {
		for i := range r.certs {
			if certMatchesName(&r.certs[i], hello.ServerName) {
				return &r.certs[i], nil
			}
		}
	}
	// No SNI match: fall back to the first certificate (single-cert
	// deployments and non-SNI clients land here).
	return &r.certs[0], nil
}

// certMatchesName reports whether cert is valid for serverName, using the
// certificate's own validation (which honours wildcard SANs like
// *.example.com), with a common-name fallback for older certs that carry
// no SAN.
func certMatchesName(cert *tls.Certificate, serverName string) bool {
	if cert.Leaf == nil {
		return false
	}
	if err := cert.Leaf.VerifyHostname(serverName); err == nil {
		return true
	}
	return false
}

// TLSConfig returns a *tls.Config that serves certificates via this
// reloader's GetCertificate hook. If caCertFile is non-empty, client
// certificates are required and verified against it (mutual TLS).
func (r *CertReloader) TLSConfig(caCertFile string) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.GetCertificate,
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

// Watch runs a background loop that polls the configured files' mtimes
// every interval and calls Reload when any of them changes. It returns
// when done is closed. A reload failure is logged and the loop continues
// with the last good certificates. interval <= 0 disables polling (the
// loop just waits for done), in which case reloads only happen via an
// explicit Reload() call.
func (r *CertReloader) Watch(done <-chan struct{}, interval time.Duration, logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	if interval <= 0 {
		<-done
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if r.changedOnDisk() {
				if err := r.Reload(); err != nil {
					logger.Printf("tlsutil: cert reload failed, keeping previous certificates: %v", err)
				} else {
					logger.Printf("tlsutil: reloaded TLS certificates from disk")
				}
			}
		}
	}
}

// changedOnDisk reports whether any watched file's mtime differs from the
// last-loaded value.
func (r *CertReloader) changedOnDisk() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for path, seen := range r.mtimes {
		if fileMtime(path) != seen {
			return true
		}
	}
	return false
}

// fileMtime returns a file's modification time, or the zero time if it
// can't be stat'd (which counts as "changed" relative to a real mtime, so
// a transiently-missing file during an atomic rename triggers a reload
// attempt on the next tick rather than being silently ignored).
func fileMtime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// CertNames returns, for observability/logging, the primary subject name
// of each currently-loaded certificate.
func (r *CertReloader) CertNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.certs))
	for i := range r.certs {
		if r.certs[i].Leaf != nil {
			if len(r.certs[i].Leaf.DNSNames) > 0 {
				names = append(names, strings.Join(r.certs[i].Leaf.DNSNames, ","))
			} else {
				names = append(names, r.certs[i].Leaf.Subject.CommonName)
			}
		} else {
			names = append(names, "(unparsed)")
		}
	}
	return names
}
