// Command dataplane runs a data plane proxy instance, in one of two
// modes selected by -protocol:
//
//   - "http" (default): an L7 HTTP reverse proxy, with L7 routing
//     (host/path/method -> backend group) and cookie-based sticky
//     sessions available.
//   - "tcp": an L4 raw TCP proxy, for non-HTTP protocols. Pinned to a
//     single backend group for its lifetime — L7 routing and sticky
//     sessions don't apply at this layer.
//
// Either way, it connects to a control plane over gRPC, subscribes to
// updates for its backend group(s), and proxies incoming traffic to
// whichever backends the control plane currently reports — no static
// config, no polling, just a live push-based backend list.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Josephfell/Jbalance/internal/config"
	"github.com/Josephfell/Jbalance/internal/dataplane"
	"github.com/Josephfell/Jbalance/internal/envflag"
	"github.com/Josephfell/Jbalance/internal/logging"
	"github.com/Josephfell/Jbalance/internal/tlsutil"
)

// fatal logs a fatal error via slog (so the message obeys the configured
// level/format like every other line) and exits non-zero, replacing the
// old log.Fatalf which wrote through the standard logger and bypassed the
// structured handler.
func fatal(msg string, args ...any) {
	slog.Error(msg, append([]any{"component", "dataplane"}, args...)...)
	os.Exit(1)
}

// Every flag below can also be set via the environment variable named in
// its usage string (e.g. from a Docker Compose env_file) — an explicitly
// passed flag always takes precedence over the environment.
func main() {
	protocol := flag.String("protocol", envflag.String("LB_PROTOCOL", "http"), "which proxy mode to run: 'http' (L7, supports routing and sticky sessions) or 'tcp' (L4, raw byte forwarding, single backend group) [env: LB_PROTOCOL]")
	listenAddr := flag.String("listen-addr", envflag.String("LB_LISTEN_ADDR", ":8080"), "address for this data plane instance to accept incoming traffic on [env: LB_LISTEN_ADDR]")
	controlPlaneAddr := flag.String("control-plane-addr", envflag.String("LB_CONTROL_PLANE_ADDR", "localhost:9090"), "address of the control plane's gRPC server [env: LB_CONTROL_PLANE_ADDR]")
	group := flag.String("group", envflag.String("LB_GROUP", "web-tier"), "backend group this data plane instance serves traffic for [env: LB_GROUP]")
	instanceID := flag.String("instance-id", envflag.String("LB_INSTANCE_ID", ""), "identifier for this instance, used only for control plane logging (defaults to hostname) [env: LB_INSTANCE_ID]")
	healthCheckInterval := flag.Duration("health-check-interval", envflag.Duration("LB_HEALTH_CHECK_INTERVAL", 5*time.Second), "how often to probe each backend [env: LB_HEALTH_CHECK_INTERVAL]")
	healthCheckTimeout := flag.Duration("health-check-timeout", envflag.Duration("LB_HEALTH_CHECK_TIMEOUT", 2*time.Second), "timeout for each backend health probe [env: LB_HEALTH_CHECK_TIMEOUT]")
	unhealthyThreshold := flag.Int("unhealthy-threshold", envflag.Int("LB_UNHEALTHY_THRESHOLD", 3), "consecutive failed probes before a backend is taken out of rotation [env: LB_UNHEALTHY_THRESHOLD]")
	healthyThreshold := flag.Int("healthy-threshold", envflag.Int("LB_HEALTHY_THRESHOLD", 2), "consecutive successful probes before a backend is returned to rotation [env: LB_HEALTHY_THRESHOLD]")

	healthCheckMode := flag.String("health-check-mode", envflag.String("LB_HEALTH_CHECK_MODE", "tcp"), "backend health probe type: 'tcp' (plain connect) or 'http' (GET a path, check the status class — catches a backend that accepts connections but serves errors) [env: LB_HEALTH_CHECK_MODE]")
	healthCheckPath := flag.String("health-check-path", envflag.String("LB_HEALTH_CHECK_PATH", "/"), "(http mode) path to GET when probing a backend [env: LB_HEALTH_CHECK_PATH]")
	healthCheckExpectStatus := flag.Int("health-check-expect-status", envflag.Int("LB_HEALTH_CHECK_EXPECT_STATUS", 0), "(http mode) exact status code a probe must return to count as healthy; 0 means any 2xx [env: LB_HEALTH_CHECK_EXPECT_STATUS]")
	healthCheckScheme := flag.String("health-check-scheme", envflag.String("LB_HEALTH_CHECK_SCHEME", "http"), "(http mode) scheme for the probe request: 'http' or 'https' [env: LB_HEALTH_CHECK_SCHEME]")
	healthCheckHost := flag.String("health-check-host", envflag.String("LB_HEALTH_CHECK_HOST", ""), "(http mode) Host header to send with the probe; defaults to the backend address [env: LB_HEALTH_CHECK_HOST]")

	cpTLSEnable := flag.Bool("control-plane-tls", envflag.Bool("LB_CONTROL_PLANE_TLS", false), "connect to the control plane over TLS [env: LB_CONTROL_PLANE_TLS]")
	cpTLSCACert := flag.String("control-plane-tls-ca", envflag.String("LB_CONTROL_PLANE_TLS_CA", ""), "CA cert to verify the control plane's TLS certificate against; if unset, the system root CA pool is used [env: LB_CONTROL_PLANE_TLS_CA]")
	cpTLSClientCert := flag.String("control-plane-tls-client-cert", envflag.String("LB_CONTROL_PLANE_TLS_CLIENT_CERT", ""), "client cert to present to the control plane (mutual TLS); requires -control-plane-tls-client-key [env: LB_CONTROL_PLANE_TLS_CLIENT_CERT]")
	cpTLSClientKey := flag.String("control-plane-tls-client-key", envflag.String("LB_CONTROL_PLANE_TLS_CLIENT_KEY", ""), "client key matching -control-plane-tls-client-cert [env: LB_CONTROL_PLANE_TLS_CLIENT_KEY]")

	httpTLSCert := flag.String("http-tls-cert", envflag.String("LB_HTTP_TLS_CERT", ""), "path to a TLS certificate for the data plane's HTTP listener; if unset, the listener runs in plaintext HTTP [env: LB_HTTP_TLS_CERT]")
	httpTLSKey := flag.String("http-tls-key", envflag.String("LB_HTTP_TLS_KEY", ""), "path to the TLS private key matching -http-tls-cert [env: LB_HTTP_TLS_KEY]")
	httpTLSCerts := flag.String("http-tls-certs", envflag.String("LB_HTTP_TLS_CERTS", ""), "comma-separated list of additional certfile:keyfile pairs for SNI (serve multiple hostnames on one listener); combined with -http-tls-cert/-http-tls-key if those are also set [env: LB_HTTP_TLS_CERTS]")
	httpTLSReloadInterval := flag.Duration("http-tls-reload-interval", envflag.Duration("LB_HTTP_TLS_RELOAD_INTERVAL", 0), "how often to check the TLS cert/key files for changes and hot-reload them without a restart; 0 disables polling (SIGHUP still forces a reload) [env: LB_HTTP_TLS_RELOAD_INTERVAL]")
	httpTLSClientCA := flag.String("http-tls-client-ca", envflag.String("LB_HTTP_TLS_CLIENT_CA", ""), "CA cert to require and verify client certificates against on the data plane's HTTP/TCP listener (mutual TLS); leave empty for server-only TLS [env: LB_HTTP_TLS_CLIENT_CA]")

	healthReportInterval := flag.Duration("health-report-interval", envflag.Duration("LB_HEALTH_REPORT_INTERVAL", 10*time.Second), "how often to report backend health status back to the control plane, for display in the admin web UI [env: LB_HEALTH_REPORT_INTERVAL]")

	// Proxy timeout / retry / draining settings (both modes where noted).
	proxyConnectTimeout := flag.Duration("proxy-connect-timeout", envflag.Duration("LB_PROXY_CONNECT_TIMEOUT", 5*time.Second), "(http mode) timeout for establishing a connection to a backend [env: LB_PROXY_CONNECT_TIMEOUT]")
	proxyResponseTimeout := flag.Duration("proxy-response-timeout", envflag.Duration("LB_PROXY_RESPONSE_TIMEOUT", 30*time.Second), "(http mode) timeout waiting for a backend's response headers; 0 disables the limit [env: LB_PROXY_RESPONSE_TIMEOUT]")
	proxyMaxRetries := flag.Int("proxy-max-retries", envflag.Int("LB_PROXY_MAX_RETRIES", 1), "(http mode) additional backends to try after a connection-level failure, for bodyless idempotent requests only [env: LB_PROXY_MAX_RETRIES]")
	proxyRetryBackoff := flag.Duration("proxy-retry-backoff", envflag.Duration("LB_PROXY_RETRY_BACKOFF", 50*time.Millisecond), "(http mode) base delay between retry attempts (linear backoff) [env: LB_PROXY_RETRY_BACKOFF]")
	tcpDialTimeout := flag.Duration("tcp-dial-timeout", envflag.Duration("LB_TCP_DIAL_TIMEOUT", 5*time.Second), "(tcp mode) timeout for connecting to the selected backend [env: LB_TCP_DIAL_TIMEOUT]")
	shutdownGrace := flag.Duration("shutdown-grace", envflag.Duration("LB_SHUTDOWN_GRACE", 5*time.Second), "how long to wait for in-flight requests/connections to drain on shutdown before forcing close [env: LB_SHUTDOWN_GRACE]")

	accessLog := flag.Bool("access-log", envflag.Bool("LB_ACCESS_LOG", false), "(http mode) write one structured access-log line per proxied request (method, path, status, latency, chosen backend, request ID) [env: LB_ACCESS_LOG]")
	accessLogFormat := flag.String("access-log-format", envflag.String("LB_ACCESS_LOG_FORMAT", "json"), "(http mode) access-log line format: 'json' or 'text' [env: LB_ACCESS_LOG_FORMAT]")

	outlierDetection := flag.Bool("outlier-detection", envflag.Bool("LB_OUTLIER_DETECTION", false), "enable passive outlier detection: eject a backend from rotation after a run of errors on real traffic, then re-admit it after a cooldown (complements active health checks) [env: LB_OUTLIER_DETECTION]")
	outlierConsecutiveErrors := flag.Int("outlier-consecutive-errors", envflag.Int("LB_OUTLIER_CONSECUTIVE_ERRORS", 5), "consecutive real-traffic errors (connection failure or 5xx) against one backend before it is passively ejected [env: LB_OUTLIER_CONSECUTIVE_ERRORS]")
	outlierEjectDuration := flag.Duration("outlier-eject-duration", envflag.Duration("LB_OUTLIER_EJECT_DURATION", 30*time.Second), "base cooldown a passively-ejected backend stays out of rotation; repeated ejections back off linearly up to -outlier-max-eject-duration [env: LB_OUTLIER_EJECT_DURATION]")
	outlierMaxEjectDuration := flag.Duration("outlier-max-eject-duration", envflag.Duration("LB_OUTLIER_MAX_EJECT_DURATION", 5*time.Minute), "cap on the backed-off ejection cooldown [env: LB_OUTLIER_MAX_EJECT_DURATION]")
	outlierMaxEjectPercent := flag.Int("outlier-max-eject-percent", envflag.Int("LB_OUTLIER_MAX_EJECT_PERCENT", 50), "safety cap: never passively eject more than this percent of a group at once, so a correlated failure can't empty the whole group [env: LB_OUTLIER_MAX_EJECT_PERCENT]")

	backendProtocol := flag.String("backend-protocol", envflag.String("LB_BACKEND_PROTOCOL", "http1"), "(http mode) protocol used to connect to backends: 'http1' (default) or 'h2c' (prior-knowledge HTTP/2 over cleartext, required to proxy gRPC backends). h2c also enables h2c on the listener so plaintext gRPC/HTTP2 clients can connect [env: LB_BACKEND_PROTOCOL]")

	metricsAddr := flag.String("metrics-addr", envflag.String("LB_METRICS_ADDR", ":9100"), "address to serve Prometheus metrics on (/metrics), separate from the traffic listener so metrics scraping never competes with proxied paths/connections [env: LB_METRICS_ADDR]")
	metricsEnabled := flag.Bool("metrics-enabled", envflag.Bool("LB_METRICS_ENABLED", true), "enable the Prometheus /metrics endpoint (set false, or LB_METRICS_ENABLED=false, to turn it off) [env: LB_METRICS_ENABLED]")
	metricsDisable := flag.Bool("metrics-disable", envflag.Bool("LB_METRICS_DISABLE", false), "disable the Prometheus /metrics endpoint entirely (legacy alias for -metrics-enabled=false; if either flag disables metrics, they are off) [env: LB_METRICS_DISABLE]")
	metricsReportInterval := flag.Duration("metrics-report-interval", envflag.Duration("LB_METRICS_REPORT_INTERVAL", 10*time.Second), "how often to push a traffic summary to the control plane, for display in the admin web UI's live charts [env: LB_METRICS_REPORT_INTERVAL]")

	opsAddr := flag.String("ops-addr", envflag.String("LB_OPS_ADDR", ":9101"), "address for the ops/health listener serving liveness (/healthz) and readiness (/readyz) probes, on its own port separate from the traffic listener (so a probe never competes with proxied traffic and works in tcp mode too); set empty to disable [env: LB_OPS_ADDR]")

	logLevel := flag.String("log-level", envflag.String("LB_LOG_LEVEL", "info"), "minimum log level: debug, info, warn, or error [env: LB_LOG_LEVEL]")
	logFormat := flag.String("log-format", envflag.String("LB_LOG_FORMAT", "text"), "structured log output format: 'json' (for log aggregation) or 'text' (human-readable, default) [env: LB_LOG_FORMAT]")

	maxConns := flag.Int("max-conns", envflag.Int("LB_MAX_CONNS", 0), "(http mode) maximum number of requests served concurrently; a request arriving while the limit is saturated is rejected with 503 rather than queued, which bounds process memory under a connection flood. 0 disables the limit [env: LB_MAX_CONNS]")
	maxHeaderBytes := flag.Int("max-header-bytes", envflag.Int("LB_MAX_HEADER_BYTES", 1<<20), "(http mode) maximum size in bytes of request headers the server will read (http.Server.MaxHeaderBytes); must be positive. Default 1MB [env: LB_MAX_HEADER_BYTES]")
	maxBodyBytes := flag.Int64("max-body-bytes", envflag.Int64("LB_MAX_BODY_BYTES", 0), "(http mode) maximum request body size in bytes; a larger body is rejected before being forwarded to a backend. 0 disables the limit [env: LB_MAX_BODY_BYTES]")
	readHeaderTimeout := flag.Duration("read-header-timeout", envflag.Duration("LB_READ_HEADER_TIMEOUT", 10*time.Second), "(http mode) how long the server waits to read a request's headers before timing out the connection (http.Server.ReadHeaderTimeout); guards against slowloris-style header-drip. Must be positive [env: LB_READ_HEADER_TIMEOUT]")
	flag.Parse()

	// Fail-fast: validate the whole configuration up front, before any
	// listener is opened, any goroutine started, or any credential loaded.
	// An invalid setting produces one clear, complete error listing every
	// problem, and a non-zero exit — instead of surfacing later as an
	// obscure runtime failure. Note this runs before logging.Setup, so a
	// bad -log-level/-log-format is itself caught here; the message goes
	// to stderr via the default logger, which is fine for a startup abort.
	if err := config.ValidateDataPlane(config.DataPlaneConfig{
		Protocol:         *protocol,
		ListenAddr:       *listenAddr,
		ControlPlaneAddr: *controlPlaneAddr,
		Group:            *group,
		MetricsAddr:      *metricsAddr,
		MetricsEnabled:   *metricsEnabled,
		MetricsDisable:   *metricsDisable,
		OpsAddr:          *opsAddr,

		HealthCheckMode:         *healthCheckMode,
		HealthCheckScheme:       *healthCheckScheme,
		HealthCheckInterval:     *healthCheckInterval,
		HealthCheckTimeout:      *healthCheckTimeout,
		HealthCheckExpectStatus: *healthCheckExpectStatus,
		UnhealthyThreshold:      *unhealthyThreshold,
		HealthyThreshold:        *healthyThreshold,

		BackendProtocol: *backendProtocol,
		AccessLog:       *accessLog,
		AccessLogFormat: *accessLogFormat,

		ProxyConnectTimeout:  *proxyConnectTimeout,
		ProxyResponseTimeout: *proxyResponseTimeout,
		ProxyMaxRetries:      *proxyMaxRetries,
		ProxyRetryBackoff:    *proxyRetryBackoff,
		TCPDialTimeout:       *tcpDialTimeout,
		ShutdownGrace:        *shutdownGrace,

		MaxConns:          *maxConns,
		MaxHeaderBytes:    *maxHeaderBytes,
		MaxBodyBytes:      *maxBodyBytes,
		ReadHeaderTimeout: *readHeaderTimeout,

		OutlierDetection:        *outlierDetection,
		OutlierConsecutiveError: *outlierConsecutiveErrors,
		OutlierEjectDuration:    *outlierEjectDuration,
		OutlierMaxEjectDuration: *outlierMaxEjectDuration,
		OutlierMaxEjectPercent:  *outlierMaxEjectPercent,

		CPTLSEnable:     *cpTLSEnable,
		CPTLSCACert:     *cpTLSCACert,
		CPTLSClientCert: *cpTLSClientCert,
		CPTLSClientKey:  *cpTLSClientKey,

		HTTPTLSCert:           *httpTLSCert,
		HTTPTLSKey:            *httpTLSKey,
		HTTPTLSCerts:          *httpTLSCerts,
		HTTPTLSClientCA:       *httpTLSClientCA,
		HTTPTLSReloadInterval: *httpTLSReloadInterval,

		LogLevel:  *logLevel,
		LogFormat: *logFormat,
	}); err != nil {
		fatal("startup configuration invalid", "error", err)
	}

	// Install the process-wide structured logger before any log line is
	// emitted, so every component (and slog-aware libraries) share one
	// level and format.
	logger := logging.Setup(logging.Config{Level: *logLevel, Format: *logFormat})
	logger = logging.Component(logger, "dataplane")

	id := *instanceID
	if id == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "dataplane"
		}
		id = hostname
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var cpTLSConfig *tls.Config
	if *cpTLSEnable {
		cfg, err := tlsutil.LoadClientConfig(*cpTLSClientCert, *cpTLSClientKey, *cpTLSCACert)
		if err != nil {
			fatal("startup error", "error", err)
		}
		cpTLSConfig = cfg
	} else {
		slog.Warn("connecting to control plane without TLS - use -control-plane-tls outside of local development", "component", "dataplane")
	}

	if *protocol != "http" && *protocol != "tcp" {
		fatal("unknown -protocol (must be 'http' or 'tcp')", "protocol", *protocol)
	}

	if *healthCheckMode != "tcp" && *healthCheckMode != "http" {
		fatal("unknown -health-check-mode (must be 'tcp' or 'http')", "mode", *healthCheckMode)
	}

	if *backendProtocol != "http1" && *backendProtocol != "h2c" {
		fatal("unknown -backend-protocol (must be 'http1' or 'h2c')", "protocol", *backendProtocol)
	}

	// -protocol, -health-check-mode and -backend-protocol are validated up
	// front by config.ValidateDataPlane (above), so by the time we reach
	// here they are known-good.

	// Assemble the TLS certificate reloader (SNI + hot-reload) from the
	// single-cert flags and/or the multi-cert SNI list. A nil reloader
	// means plaintext.
	var certReloader *tlsutil.CertReloader
	{
		var pairs []tlsutil.CertPair
		if *httpTLSCert != "" && *httpTLSKey != "" {
			pairs = append(pairs, tlsutil.CertPair{CertFile: *httpTLSCert, KeyFile: *httpTLSKey})
		}
		extra, err := tlsutil.ParseCertPairs(*httpTLSCerts)
		if err != nil {
			fatal("startup error", "error", err)
		}
		pairs = append(pairs, extra...)
		if len(pairs) > 0 {
			certReloader, err = tlsutil.NewCertReloader(pairs)
			if err != nil {
				fatal("startup error", "error", err)
			}
			slog.Info("TLS listener enabled", "component", "dataplane", "certs", len(pairs), "names", certReloader.CertNames())
			// Hot-reload on file change (if a poll interval is set) and on
			// SIGHUP, so a rotated cert is picked up without a restart.
			done := make(chan struct{})
			go func() {
				<-ctx.Done()
				close(done)
			}()
			go certReloader.Watch(done, *httpTLSReloadInterval, nil)
			go func() {
				hup := make(chan os.Signal, 1)
				signal.Notify(hup, syscall.SIGHUP)
				for {
					select {
					case <-ctx.Done():
						return
					case <-hup:
						if err := certReloader.Reload(); err != nil {
							slog.Error("SIGHUP cert reload failed, keeping previous certificates", "component", "dataplane", "error", err)
						} else {
							slog.Info("SIGHUP reloaded TLS certificates", "component", "dataplane", "names", certReloader.CertNames())
						}
					}
				}
			}()
		}
	}

	// groups owns one BackendList (+ Subscriber, HealthChecker,
	// HealthReporter) per backend group this instance ends up proxying
	// to. -group's subscription is started eagerly below so an instance
	// with no L7 routes configured behaves exactly as before; any
	// additional group referenced by a route rule is started lazily, the
	// first time a request actually resolves to it. (L4/tcp mode never
	// references any group beyond -group — routing is an L7-only concept
	// — but reuses the same GroupManager for its health
	// checking/reporting.)
	groups := dataplane.NewGroupManager(ctx, *controlPlaneAddr, id, cpTLSConfig, dataplane.HealthCheckConfig{
		Interval:         *healthCheckInterval,
		Timeout:          *healthCheckTimeout,
		FailureThreshold: *unhealthyThreshold,
		SuccessThreshold: *healthyThreshold,
		Mode:             dataplane.HealthCheckMode(*healthCheckMode),
		HTTPPath:         *healthCheckPath,
		HTTPExpectStatus: *healthCheckExpectStatus,
		HTTPScheme:       *healthCheckScheme,
		HTTPHost:         *healthCheckHost,
	}, *healthReportInterval)
	groups.SetOutlierConfig(dataplane.OutlierConfig{
		Enabled:           *outlierDetection,
		ConsecutiveErrors: *outlierConsecutiveErrors,
		BaseEjectDuration: *outlierEjectDuration,
		MaxEjectDuration:  *outlierMaxEjectDuration,
		MaxEjectPercent:   *outlierMaxEjectPercent,
	})
	if *outlierDetection {
		slog.Info("passive outlier detection enabled", "component", "dataplane", "consecutive_errors", *outlierConsecutiveErrors, "eject_duration", *outlierEjectDuration, "max_eject_percent", *outlierMaxEjectPercent)
	}
	defaultBackends := groups.Ensure(*group)

	var metrics *dataplane.Metrics
	metricsOn := *metricsEnabled && !*metricsDisable
	if metricsOn {
		registry := prometheus.NewRegistry()
		metrics = dataplane.NewMetrics(registry)
		dataplane.RegisterBackendsCollector(registry, groups)
		startMetricsServer(ctx, *metricsAddr, registry)

		// Push a lightweight summary to the control plane too, so the
		// admin web UI's live charts have data without needing a
		// Prometheus server scraping this instance's /metrics — the
		// control plane has no route back to reach this instance
		// directly (only the reverse connection exists).
		metricsReporter := dataplane.NewMetricsReporter(*controlPlaneAddr, id, metrics, cpTLSConfig, *metricsReportInterval)
		go metricsReporter.Run(ctx)
	} else {
		slog.Info("metrics endpoint disabled", "component", "dataplane")
	}

	// Ops/health listener: liveness (/healthz) and readiness (/readyz) on
	// their own port, separate from the traffic listener — so a
	// kubelet/LB probe never competes with proxied traffic, and so tcp
	// (L4) mode gets probe endpoints even though it stands up no HTTP
	// traffic server of its own. Liveness just proves the process is
	// alive; readiness proves config has loaded AND at least one backend
	// is healthy across all tracked groups (nothing to route to => not
	// ready).
	ready := func() bool {
		return groups.HealthyLen() > 0
	}
	startOpsServer(ctx, *opsAddr, ready)

	if *protocol == "tcp" {
		runTCP(ctx, id, *group, *listenAddr, *controlPlaneAddr, defaultBackends, metrics, certReloader, *httpTLSClientCA, *tcpDialTimeout, *shutdownGrace)
		return
	}

	routes := dataplane.NewRouteTable(*group)
	routeSub := dataplane.NewRouteSubscriber(*controlPlaneAddr, id, routes, cpTLSConfig)
	go routeSub.Run(ctx)

	proxy := dataplane.NewProxy(routes, groups, metrics, dataplane.ProxyConfig{
		ConnectTimeout:  *proxyConnectTimeout,
		ResponseTimeout: *proxyResponseTimeout,
		MaxRetries:      *proxyMaxRetries,
		RetryBackoff:    *proxyRetryBackoff,
		BackendProtocol: *backendProtocol,
	})

	mux := http.NewServeMux()
	accessLogCfg := dataplane.AccessLogConfig{
		Enabled: *accessLog,
		Format:  dataplane.AccessLogFormat(*accessLogFormat),
	}
	// Handler chain, outermost first:
	//   AccessLog (assigns request-id + context logger)
	//     -> Recovery (catches panics, logs with request-id, 500s)
	//       -> MaxConns (bounds concurrency, 503s when saturated)
	//         -> MaxBody (caps request body size)
	//           -> proxy
	// Recovery sits INSIDE AccessLog so a recovered 500 is still access-logged
	// and the panic log line carries the request-id; it sits OUTSIDE the
	// resource-limit middlewares so a panic in either is also caught.
	handler := proxy.Handler()
	handler = dataplane.MaxBodyMiddleware(handler, *maxBodyBytes)
	handler = dataplane.MaxConnsMiddleware(handler, *maxConns)
	handler = dataplane.RecoveryMiddleware(handler, *group, metrics)
	mux.Handle("/", dataplane.AccessLogMiddleware(handler, accessLogCfg, nil, logger))
	if *maxConns > 0 {
		slog.Info("max concurrent connections limit enabled", "component", "dataplane", "max_conns", *maxConns)
	}
	if *maxBodyBytes > 0 {
		slog.Info("max request body size limit enabled", "component", "dataplane", "max_body_bytes", *maxBodyBytes)
	}
	if *accessLog {
		slog.Info("access logging enabled", "component", "dataplane", "format", *accessLogFormat)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			slog.Error("failed to write /healthz response", "component", "dataplane", "error", err)
		}
	})
	mux.HandleFunc("/debug/backends", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		body := "default group: " + *group + "\n" +
			"backend count: " + strconv.Itoa(defaultBackends.Len()) + "\n" +
			"healthy count: " + strconv.Itoa(defaultBackends.HealthyLen()) + "\n" +
			"version: " + strconv.FormatInt(defaultBackends.Version(), 10) + "\n" +
			"routes configured: " + strconv.Itoa(routes.Len()) + "\n" +
			"tracked groups: " + strings.Join(groups.Groups(), ", ") + "\n"
		if _, err := w.Write([]byte(body)); err != nil {
			slog.Error("failed to write /debug/backends response", "component", "dataplane", "error", err)
		}
	})

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: *readHeaderTimeout,
		MaxHeaderBytes:    *maxHeaderBytes,
	}
	if *backendProtocol == "h2c" {
		// Serve unencrypted HTTP/2 (h2c) on the same listener, so a
		// plaintext gRPC/HTTP2 client can reach the proxy, while ordinary
		// HTTP/1.1 clients keep working on the same port. Uses the stdlib
		// Protocols API (Go 1.24+) rather than the deprecated h2c handler
		// wrapper. When TLS is enabled, HTTP/2 is negotiated via ALPN
		// instead and this has no effect on the plaintext path.
		var protos http.Protocols
		protos.SetHTTP1(true)
		protos.SetUnencryptedHTTP2(true)
		server.Protocols = &protos
		slog.Info("h2c enabled on listener (gRPC/HTTP2 cleartext)", "component", "dataplane")
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down HTTP server, draining in-flight requests", "component", "dataplane", "grace", *shutdownGrace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("instance serving", "component", "dataplane", "instance", id, "group", *group, "listen", *listenAddr, "control_plane", *controlPlaneAddr)

	var serveErr error
	if certReloader != nil {
		tlsCfg, err := certReloader.TLSConfig(*httpTLSClientCA)
		if err != nil {
			fatal("startup error", "error", err)
		}
		server.TLSConfig = tlsCfg
		ln, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			fatal("failed to listen", "addr", *listenAddr, "error", err)
		}
		slog.Info("HTTP listener TLS enabled (SNI + hot-reload)", "component", "dataplane")
		// Empty cert/key args: the certificates come from TLSConfig.GetCertificate.
		serveErr = server.ServeTLS(ln, "", "")
	} else {
		slog.Warn("HTTP listener running without TLS - use -http-tls-cert/-http-tls-key outside of local development", "component", "dataplane")
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		fatal("HTTP server error", "error", serveErr)
	}
}

// runTCP runs the L4 (raw TCP) proxy loop. Unlike the HTTP path, there is
// no L7 routing (a TCP proxy has no visibility into what's inside the
// bytes it forwards) and no debug/healthz HTTP endpoints — group is the
// only backend group this listener will ever proxy to, for its entire
// lifetime, exactly like an L7 data plane instance before routing existed.
func runTCP(ctx context.Context, id, group, listenAddr, controlPlaneAddr string, backends *dataplane.BackendList, metrics *dataplane.Metrics, certReloader *tlsutil.CertReloader, clientCA string, dialTimeout, shutdownGrace time.Duration) {
	var ln net.Listener
	var err error
	if certReloader != nil {
		tlsCfg, cfgErr := certReloader.TLSConfig(clientCA)
		if cfgErr != nil {
			fatal("startup error", "error", cfgErr)
		}
		slog.Info("TCP listener TLS enabled (SNI + hot-reload)", "component", "dataplane")
		ln, err = tls.Listen("tcp", listenAddr, tlsCfg)
	} else {
		slog.Warn("TCP listener running without TLS - use -http-tls-cert/-http-tls-key outside of local development", "component", "dataplane")
		ln, err = net.Listen("tcp", listenAddr)
	}
	if err != nil {
		fatal("failed to listen", "addr", listenAddr, "error", err)
	}

	proxy := dataplane.NewTCPProxy(group, backends, metrics)
	proxy.DialTimeout = dialTimeout

	go func() {
		<-ctx.Done()
		slog.Info("shutting down TCP listener, draining in-flight connections", "component", "dataplane", "grace", shutdownGrace)
		// Stop accepting new connections first, then let existing ones
		// drain up to the grace period.
		_ = ln.Close()
		if !proxy.Drain(shutdownGrace) {
			slog.Warn("TCP drain grace period elapsed with connections still in flight", "component", "dataplane")
		}
	}()

	slog.Info("instance serving (tcp)", "component", "dataplane", "instance", id, "group", group, "listen", listenAddr, "control_plane", controlPlaneAddr)

	if err := proxy.Serve(ctx, ln); err != nil {
		fatal("TCP proxy error", "error", err)
	}
}

// startMetricsServer starts a small dedicated HTTP server exposing
// registry via /metrics, on its own listener separate from the traffic
// port — deliberate, so a Prometheus scrape can never compete with (or
// be confused for) actual proxied HTTP requests, and so the L4/tcp mode
// (which otherwise has no HTTP server at all) still gets metrics without
// needing one stood up just for this. Runs in the background; a failure
// to bind is logged but not fatal, since metrics are an observability
// nice-to-have, not something that should take down request proxying.
func startMetricsServer(ctx context.Context, addr string, registry *prometheus.Registry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		slog.Info("metrics endpoint listening", "component", "dataplane", "addr", addr+"/metrics")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server error", "component", "dataplane", "error", err)
		}
	}()
}

// startOpsServer stands up a small dedicated HTTP listener for Kubernetes
// (or any orchestrator / external load balancer) probe endpoints, on its
// own port separate from both the traffic listener and the metrics
// listener:
//
//   - GET /healthz — liveness. Always 200 while the process is running.
//     A failing liveness probe tells the orchestrator to RESTART the pod,
//     so it must only report the process itself being wedged, never a
//     transient lack of backends (which a restart would not fix).
//   - GET /readyz — readiness. 200 when ready() is true, 503 otherwise.
//     ready() reports whether config has loaded and at least one backend
//     is healthy, so a failing readiness probe pulls this instance OUT of
//     the service's endpoints (no traffic) without restarting it, and it
//     rejoins automatically once a backend goes healthy again.
//
// Deliberately on its own port so a probe can never compete with proxied
// traffic, and so tcp (L4) mode — which stands up no HTTP traffic server
// of its own — still gets probe endpoints. Passing an empty addr disables
// it. A bind failure is logged, not fatal: probes are an operational
// aid, not something that should take down request proxying.
func startOpsServer(ctx context.Context, addr string, ready func() bool) {
	if addr == "" {
		slog.Info("ops/health listener disabled (empty -ops-addr)", "component", "dataplane")
		return
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           opsMux(ready),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		slog.Info("ops/health listener", "component", "dataplane", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("ops/health server error", "component", "dataplane", "error", err)
		}
	}()
}

// opsMux builds the handler for the ops/health listener. Split out from
// startOpsServer so the probe behaviour can be exercised in tests without
// binding a real port. ready reports readiness; a nil ready is treated as
// never ready.
func opsMux(ready func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			slog.Error("failed to write /healthz response", "component", "dataplane", "error", err)
		}
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if ready != nil && ready() {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("ready")); err != nil {
				slog.Error("failed to write /readyz response", "component", "dataplane", "error", err)
			}
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write([]byte("not ready: no healthy backends")); err != nil {
			slog.Error("failed to write /readyz response", "component", "dataplane", "error", err)
		}
	})
	return mux
}
