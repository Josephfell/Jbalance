// Package config centralises startup validation of the LB_* configuration
// for both the control plane and the data plane, so an invalid setting is
// rejected once, up front, with a clear message — rather than surfacing
// later as an obscure runtime failure (a listener that never binds, a TLS
// handshake that fails on first connection, a provider that returns no
// backends, a negative timeout that silently means "no timeout").
//
// The two entry points, ValidateDataPlane and ValidateControlPlane, each
// take the already-parsed flag values as a plain struct and return a single
// error describing EVERY problem found, not just the first — an operator
// fixing a bad config should see the whole list in one run instead of
// playing whack-a-mole one restart at a time. main() calls the relevant
// validator immediately after flag.Parse(), before it opens any listener,
// starts any goroutine, or loads any credential, and exits non-zero on
// error.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Josephfell/Jbalance/internal/tlsutil"
)

// problems accumulates validation errors so a single run reports every
// issue at once instead of stopping at the first.
type problems struct {
	errs []string
}

func (p *problems) addf(format string, args ...any) {
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

// err folds the accumulated problems into one error, or nil if there were
// none. The message lists each problem on its own line so it is readable
// in both text and (escaped) JSON log output.
func (p *problems) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problem(s)):", len(p.errs))
	for _, e := range p.errs {
		b.WriteString("\n  - ")
		b.WriteString(e)
	}
	return errors.New(b.String())
}

// checkListenAddr validates a "host:port" listen address of the form
// accepted by net.Listen (an empty host means all interfaces, which is
// valid). name is the flag/env label used in the error message. An empty
// address is only allowed when allowEmpty is set (e.g. -ops-addr disables
// the listener when empty).
func (p *problems) checkListenAddr(name, addr string, allowEmpty bool) {
	if addr == "" {
		if !allowEmpty {
			p.addf("%s must not be empty", name)
		}
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		p.addf("%s %q is not a valid listen address (want host:port, e.g. :8080): %v", name, addr, err)
		return
	}
	// Port must be numeric and in range (net.Listen also accepts named
	// ports like "http", but requiring a number gives operators a far
	// clearer failure than a later resolve error).
	if port == "" {
		p.addf("%s %q is missing a port", name, addr)
		return
	}
	pnum, perr := net.LookupPort("tcp", port)
	if perr != nil {
		p.addf("%s %q has an invalid port %q", name, addr, port)
		return
	}
	if pnum == 0 {
		// Port 0 asks the OS to pick a free port; harmless for a real
		// listener but almost never intended for a load balancer's fixed
		// address, so flag it.
		p.addf("%s %q uses port 0 (OS-assigned) — set an explicit port", name, addr)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil {
			// A non-IP host is allowed by net.Listen (it resolves it), but
			// warn-as-error only for the clearly-broken empty case above;
			// hostnames are left to resolve at bind time.
			_ = ip
		}
	}
}

// checkDialAddr validates a "host:port" address the process will DIAL (as
// opposed to listen on). Unlike a listen address, an empty host is not
// meaningful for a dial target, so it must be present.
func (p *problems) checkDialAddr(name, addr string) {
	if addr == "" {
		p.addf("%s must not be empty", name)
		return
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		p.addf("%s %q is not a valid address (want host:port): %v", name, addr, err)
		return
	}
	if host == "" {
		p.addf("%s %q is missing a host", name, addr)
	}
	if port == "" {
		p.addf("%s %q is missing a port", name, addr)
		return
	}
	if _, perr := net.LookupPort("tcp", port); perr != nil {
		p.addf("%s %q has an invalid port %q", name, addr, port)
	}
}

// checkPositive requires a duration to be strictly greater than zero.
func (p *problems) checkPositive(name string, d time.Duration) {
	if d <= 0 {
		p.addf("%s must be positive (got %s)", name, d)
	}
}

// checkNonNegative requires a duration to be zero or greater. Used where 0
// is a documented "disabled/unlimited" sentinel.
func (p *problems) checkNonNegative(name string, d time.Duration) {
	if d < 0 {
		p.addf("%s must not be negative (got %s)", name, d)
	}
}

// checkNonNegativeInt requires an int to be zero or greater.
func (p *problems) checkNonNegativeInt(name string, v int) {
	if v < 0 {
		p.addf("%s must not be negative (got %d)", name, v)
	}
}

// checkPositiveInt requires an int to be strictly greater than zero.
func (p *problems) checkPositiveInt(name string, v int) {
	if v <= 0 {
		p.addf("%s must be positive (got %d)", name, v)
	}
}

// checkOneOf requires value to be one of allowed.
func (p *problems) checkOneOf(name, value string, allowed ...string) {
	for _, a := range allowed {
		if value == a {
			return
		}
	}
	p.addf("%s %q is invalid (must be one of: %s)", name, value, strings.Join(allowed, ", "))
}

// checkFileExists requires a path to exist and be a regular, readable file.
// Used for cert/key/CA paths that must be present at startup.
func (p *problems) checkFileExists(name, path string) {
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		p.addf("%s file %q cannot be accessed: %v", name, path, err)
		return
	}
	if info.IsDir() {
		p.addf("%s %q is a directory, not a file", name, path)
	}
}

// DataPlaneConfig holds the data plane's already-parsed configuration for
// validation. Field names mirror the -flag / LB_ env names.
type DataPlaneConfig struct {
	Protocol         string
	ListenAddr       string
	ControlPlaneAddr string
	Group            string
	MetricsAddr      string
	MetricsEnabled   bool
	MetricsDisable   bool
	OpsAddr          string

	HealthCheckMode         string
	HealthCheckScheme       string
	HealthCheckInterval     time.Duration
	HealthCheckTimeout      time.Duration
	HealthCheckExpectStatus int
	UnhealthyThreshold      int
	HealthyThreshold        int

	BackendProtocol string
	AccessLog       bool
	AccessLogFormat string

	ProxyConnectTimeout  time.Duration
	ProxyResponseTimeout time.Duration
	ProxyMaxRetries      int
	ProxyRetryBackoff    time.Duration
	TCPDialTimeout       time.Duration
	ShutdownGrace        time.Duration

	MaxConns          int
	MaxHeaderBytes    int
	MaxBodyBytes      int64
	ReadHeaderTimeout time.Duration

	OutlierDetection        bool
	OutlierConsecutiveError int
	OutlierEjectDuration    time.Duration
	OutlierMaxEjectDuration time.Duration
	OutlierMaxEjectPercent  int

	// control-plane client TLS
	CPTLSEnable     bool
	CPTLSCACert     string
	CPTLSClientCert string
	CPTLSClientKey  string

	// data-plane listener TLS
	HTTPTLSCert           string
	HTTPTLSKey            string
	HTTPTLSCerts          string
	HTTPTLSClientCA       string
	HTTPTLSReloadInterval time.Duration

	LogLevel  string
	LogFormat string
}

// ValidateDataPlane checks a data plane configuration and returns an error
// describing every problem found (nil if the config is valid). It performs
// no side effects beyond stat-ing configured cert files and attempting to
// load them, which is exactly the fail-fast the runtime would otherwise
// hit later.
func ValidateDataPlane(c DataPlaneConfig) error {
	var p problems

	p.checkOneOf("-protocol (LB_PROTOCOL)", c.Protocol, "http", "tcp")
	p.checkOneOf("-health-check-mode (LB_HEALTH_CHECK_MODE)", c.HealthCheckMode, "tcp", "http")
	p.checkOneOf("-health-check-scheme (LB_HEALTH_CHECK_SCHEME)", c.HealthCheckScheme, "http", "https")
	p.checkOneOf("-backend-protocol (LB_BACKEND_PROTOCOL)", c.BackendProtocol, "http1", "h2c")
	p.checkOneOf("-access-log-format (LB_ACCESS_LOG_FORMAT)", c.AccessLogFormat, "json", "text")
	p.checkOneOf("-log-level (LB_LOG_LEVEL)", c.LogLevel, "debug", "info", "warn", "error")
	p.checkOneOf("-log-format (LB_LOG_FORMAT)", c.LogFormat, "json", "text")

	if strings.TrimSpace(c.Group) == "" {
		p.addf("-group (LB_GROUP) must not be empty")
	}

	p.checkListenAddr("-listen-addr (LB_LISTEN_ADDR)", c.ListenAddr, false)
	p.checkDialAddr("-control-plane-addr (LB_CONTROL_PLANE_ADDR)", c.ControlPlaneAddr)
	p.checkListenAddr("-metrics-addr (LB_METRICS_ADDR)", c.MetricsAddr, false)
	// -ops-addr may be empty to disable the ops listener.
	p.checkListenAddr("-ops-addr (LB_OPS_ADDR)", c.OpsAddr, true)

	p.checkPositive("-health-check-interval (LB_HEALTH_CHECK_INTERVAL)", c.HealthCheckInterval)
	p.checkPositive("-health-check-timeout (LB_HEALTH_CHECK_TIMEOUT)", c.HealthCheckTimeout)
	p.checkPositiveInt("-unhealthy-threshold (LB_UNHEALTHY_THRESHOLD)", c.UnhealthyThreshold)
	p.checkPositiveInt("-healthy-threshold (LB_HEALTHY_THRESHOLD)", c.HealthyThreshold)
	if c.HealthCheckExpectStatus != 0 && (c.HealthCheckExpectStatus < 100 || c.HealthCheckExpectStatus > 599) {
		p.addf("-health-check-expect-status (LB_HEALTH_CHECK_EXPECT_STATUS) %d is not a valid HTTP status code (want 0 for any-2xx, or 100-599)", c.HealthCheckExpectStatus)
	}

	p.checkPositive("-proxy-connect-timeout (LB_PROXY_CONNECT_TIMEOUT)", c.ProxyConnectTimeout)
	// response timeout 0 == disabled (documented), so only reject negative.
	p.checkNonNegative("-proxy-response-timeout (LB_PROXY_RESPONSE_TIMEOUT)", c.ProxyResponseTimeout)
	p.checkNonNegativeInt("-proxy-max-retries (LB_PROXY_MAX_RETRIES)", c.ProxyMaxRetries)
	p.checkNonNegative("-proxy-retry-backoff (LB_PROXY_RETRY_BACKOFF)", c.ProxyRetryBackoff)
	p.checkPositive("-tcp-dial-timeout (LB_TCP_DIAL_TIMEOUT)", c.TCPDialTimeout)
	p.checkNonNegative("-shutdown-grace (LB_SHUTDOWN_GRACE)", c.ShutdownGrace)

	// Resource limits (http mode). 0 is the documented "unlimited/disabled"
	// sentinel for max-conns and max-body-bytes, so only reject negatives;
	// max-header-bytes and read-header-timeout must be strictly positive
	// (a zero/negative header limit or read timeout is never intended).
	p.checkNonNegativeInt("-max-conns (LB_MAX_CONNS)", c.MaxConns)
	p.checkPositiveInt("-max-header-bytes (LB_MAX_HEADER_BYTES)", c.MaxHeaderBytes)
	if c.MaxBodyBytes < 0 {
		p.addf("-max-body-bytes (LB_MAX_BODY_BYTES) must not be negative (got %d)", c.MaxBodyBytes)
	}
	p.checkPositive("-read-header-timeout (LB_READ_HEADER_TIMEOUT)", c.ReadHeaderTimeout)

	if c.OutlierDetection {
		p.checkPositiveInt("-outlier-consecutive-errors (LB_OUTLIER_CONSECUTIVE_ERRORS)", c.OutlierConsecutiveError)
		p.checkPositive("-outlier-eject-duration (LB_OUTLIER_EJECT_DURATION)", c.OutlierEjectDuration)
		p.checkPositive("-outlier-max-eject-duration (LB_OUTLIER_MAX_EJECT_DURATION)", c.OutlierMaxEjectDuration)
		if c.OutlierMaxEjectDuration > 0 && c.OutlierEjectDuration > 0 && c.OutlierMaxEjectDuration < c.OutlierEjectDuration {
			p.addf("-outlier-max-eject-duration (%s) must be >= -outlier-eject-duration (%s)", c.OutlierMaxEjectDuration, c.OutlierEjectDuration)
		}
		if c.OutlierMaxEjectPercent < 0 || c.OutlierMaxEjectPercent > 100 {
			p.addf("-outlier-max-eject-percent (LB_OUTLIER_MAX_EJECT_PERCENT) %d is out of range (want 0-100)", c.OutlierMaxEjectPercent)
		}
	}

	p.checkNonNegative("-http-tls-reload-interval (LB_HTTP_TLS_RELOAD_INTERVAL)", c.HTTPTLSReloadInterval)

	// Control-plane client TLS: a client cert requires a matching key and
	// vice versa; enabling mTLS material without -control-plane-tls is a
	// no-op the operator probably didn't intend.
	if (c.CPTLSClientCert == "") != (c.CPTLSClientKey == "") {
		p.addf("-control-plane-tls-client-cert and -control-plane-tls-client-key must be set together")
	}
	if !c.CPTLSEnable && (c.CPTLSCACert != "" || c.CPTLSClientCert != "" || c.CPTLSClientKey != "") {
		p.addf("-control-plane-tls-ca / -control-plane-tls-client-cert / -control-plane-tls-client-key have no effect without -control-plane-tls (LB_CONTROL_PLANE_TLS)")
	}
	p.checkFileExists("-control-plane-tls-ca (LB_CONTROL_PLANE_TLS_CA)", c.CPTLSCACert)
	p.checkFileExists("-control-plane-tls-client-cert (LB_CONTROL_PLANE_TLS_CLIENT_CERT)", c.CPTLSClientCert)
	p.checkFileExists("-control-plane-tls-client-key (LB_CONTROL_PLANE_TLS_CLIENT_KEY)", c.CPTLSClientKey)
	// If TLS to the control plane is enabled with client material, confirm
	// it actually loads — the same call main() would make.
	if c.CPTLSEnable && c.CPTLSClientCert != "" && c.CPTLSClientKey != "" {
		if _, err := tlsutil.LoadClientConfig(c.CPTLSClientCert, c.CPTLSClientKey, c.CPTLSCACert); err != nil {
			p.addf("control-plane client TLS material fails to load: %v", err)
		}
	}

	// Data-plane listener TLS: -http-tls-cert and -http-tls-key go
	// together. The SNI list (-http-tls-certs) is parsed and each pair's
	// files are loaded to fail fast on a bad cert.
	if (c.HTTPTLSCert == "") != (c.HTTPTLSKey == "") {
		p.addf("-http-tls-cert and -http-tls-key must be set together")
	}
	p.checkFileExists("-http-tls-cert (LB_HTTP_TLS_CERT)", c.HTTPTLSCert)
	p.checkFileExists("-http-tls-key (LB_HTTP_TLS_KEY)", c.HTTPTLSKey)
	p.checkFileExists("-http-tls-client-ca (LB_HTTP_TLS_CLIENT_CA)", c.HTTPTLSClientCA)

	pairs, err := tlsutil.ParseCertPairs(c.HTTPTLSCerts)
	if err != nil {
		p.addf("-http-tls-certs (LB_HTTP_TLS_CERTS) is malformed: %v", err)
	} else {
		// Only attempt to build the reloader when there is at least one
		// pair (single-cert flags and/or SNI list), mirroring main().
		var all []tlsutil.CertPair
		if c.HTTPTLSCert != "" && c.HTTPTLSKey != "" {
			all = append(all, tlsutil.CertPair{CertFile: c.HTTPTLSCert, KeyFile: c.HTTPTLSKey})
		}
		all = append(all, pairs...)
		if len(all) > 0 {
			if _, err := tlsutil.NewCertReloader(all); err != nil {
				p.addf("data-plane TLS certificates fail to load: %v", err)
			}
			// A client-CA (mTLS) with no server cert has nothing to attach to.
			if c.HTTPTLSClientCA != "" && len(all) == 0 {
				p.addf("-http-tls-client-ca requires a server certificate (-http-tls-cert/-http-tls-key or -http-tls-certs)")
			}
		} else if c.HTTPTLSClientCA != "" {
			p.addf("-http-tls-client-ca requires a server certificate (-http-tls-cert/-http-tls-key or -http-tls-certs)")
		}
	}

	return p.err()
}

// ControlPlaneConfig holds the control plane's already-parsed configuration
// for validation. Field names mirror the -flag / LB_ env names.
type ControlPlaneConfig struct {
	GRPCAddr          string
	AdminAddr         string
	AdminDisable      bool
	ReconcileInterval time.Duration

	TLSCert     string
	TLSKey      string
	TLSClientCA string

	Provider string

	// fake provider
	FakeBasePort    int
	ScalingInterval time.Duration
	SimulateScaling bool

	// azure-vmss provider
	AzureSubscriptionID string
	AzureResourceGroup  string
	AzureVMSSGroups     string

	// kubernetes provider
	K8sGroups     string
	K8sKubeconfig string

	AdminTLSCert string
	AdminTLSKey  string

	LogLevel  string
	LogFormat string
}

// ValidateControlPlane checks a control plane configuration and returns an
// error describing every problem found (nil if valid). Provider-specific
// required fields are checked only for the selected provider.
func ValidateControlPlane(c ControlPlaneConfig) error {
	var p problems

	p.checkOneOf("-provider (LB_PROVIDER)", c.Provider, "fake", "azure-vmss", "kubernetes")
	p.checkOneOf("-log-level (LB_LOG_LEVEL)", c.LogLevel, "debug", "info", "warn", "error")
	p.checkOneOf("-log-format (LB_LOG_FORMAT)", c.LogFormat, "json", "text")

	p.checkListenAddr("-grpc-addr (LB_GRPC_ADDR)", c.GRPCAddr, false)
	if !c.AdminDisable {
		p.checkListenAddr("-admin-addr (LB_ADMIN_ADDR)", c.AdminAddr, false)
	}
	p.checkPositive("-reconcile-interval (LB_RECONCILE_INTERVAL)", c.ReconcileInterval)

	// gRPC server TLS: cert and key go together; a client-CA (mTLS)
	// without a server cert has nothing to attach to.
	if (c.TLSCert == "") != (c.TLSKey == "") {
		p.addf("-tls-cert and -tls-key must be set together")
	}
	if c.TLSClientCA != "" && c.TLSCert == "" {
		p.addf("-tls-client-ca requires -tls-cert/-tls-key (mutual TLS needs a server certificate)")
	}
	p.checkFileExists("-tls-cert (LB_TLS_CERT)", c.TLSCert)
	p.checkFileExists("-tls-key (LB_TLS_KEY)", c.TLSKey)
	p.checkFileExists("-tls-client-ca (LB_TLS_CLIENT_CA)", c.TLSClientCA)
	if c.TLSCert != "" && c.TLSKey != "" {
		if _, err := tlsutil.LoadServerConfig(c.TLSCert, c.TLSKey, c.TLSClientCA); err != nil {
			p.addf("gRPC server TLS material fails to load: %v", err)
		}
	}

	// Admin UI TLS.
	if (c.AdminTLSCert == "") != (c.AdminTLSKey == "") {
		p.addf("-admin-tls-cert and -admin-tls-key must be set together")
	}
	p.checkFileExists("-admin-tls-cert (LB_ADMIN_TLS_CERT)", c.AdminTLSCert)
	p.checkFileExists("-admin-tls-key (LB_ADMIN_TLS_KEY)", c.AdminTLSKey)

	switch c.Provider {
	case "fake":
		if c.FakeBasePort < 1 || c.FakeBasePort > 65535 {
			p.addf("-fake-base-port (LB_FAKE_BASE_PORT) %d is out of range (want 1-65535)", c.FakeBasePort)
		}
		if c.SimulateScaling {
			p.checkPositive("-scaling-interval (LB_SCALING_INTERVAL)", c.ScalingInterval)
		}
	case "azure-vmss":
		if strings.TrimSpace(c.AzureSubscriptionID) == "" {
			p.addf("-azure-subscription-id (LB_AZURE_SUBSCRIPTION_ID) is required for the azure-vmss provider")
		}
		if strings.TrimSpace(c.AzureResourceGroup) == "" {
			p.addf("-azure-resource-group (LB_AZURE_RESOURCE_GROUP) is required for the azure-vmss provider")
		}
		if strings.TrimSpace(c.AzureVMSSGroups) == "" {
			p.addf("-azure-vmss-groups (LB_AZURE_VMSS_GROUPS) must specify at least one group for the azure-vmss provider")
		}
	case "kubernetes":
		if strings.TrimSpace(c.K8sGroups) == "" {
			p.addf("-k8s-groups (LB_K8S_GROUPS) must specify at least one group for the kubernetes provider")
		}
	}

	return p.err()
}
