// Command dataplane runs an L7 HTTP reverse proxy instance. It connects to
// a control plane over gRPC, subscribes to updates for a named backend
// group, and proxies incoming HTTP requests to whichever backends the
// control plane currently reports for that group — no static config, no
// polling, just a live push-based backend list.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Josephfell/Jbalance/internal/dataplane"
	"github.com/Josephfell/Jbalance/internal/envflag"
	"github.com/Josephfell/Jbalance/internal/tlsutil"
)

// Every flag below can also be set via the environment variable named in
// its usage string (e.g. from a Docker Compose env_file) — an explicitly
// passed flag always takes precedence over the environment.
func main() {
	listenAddr := flag.String("listen-addr", envflag.String("LB_LISTEN_ADDR", ":8080"), "address for this data plane instance to accept incoming traffic on [env: LB_LISTEN_ADDR]")
	controlPlaneAddr := flag.String("control-plane-addr", envflag.String("LB_CONTROL_PLANE_ADDR", "localhost:9090"), "address of the control plane's gRPC server [env: LB_CONTROL_PLANE_ADDR]")
	group := flag.String("group", envflag.String("LB_GROUP", "web-tier"), "backend group this data plane instance serves traffic for [env: LB_GROUP]")
	instanceID := flag.String("instance-id", envflag.String("LB_INSTANCE_ID", ""), "identifier for this instance, used only for control plane logging (defaults to hostname) [env: LB_INSTANCE_ID]")
	healthCheckInterval := flag.Duration("health-check-interval", envflag.Duration("LB_HEALTH_CHECK_INTERVAL", 5*time.Second), "how often to probe each backend [env: LB_HEALTH_CHECK_INTERVAL]")
	healthCheckTimeout := flag.Duration("health-check-timeout", envflag.Duration("LB_HEALTH_CHECK_TIMEOUT", 2*time.Second), "timeout for each backend health probe [env: LB_HEALTH_CHECK_TIMEOUT]")
	unhealthyThreshold := flag.Int("unhealthy-threshold", envflag.Int("LB_UNHEALTHY_THRESHOLD", 3), "consecutive failed probes before a backend is taken out of rotation [env: LB_UNHEALTHY_THRESHOLD]")
	healthyThreshold := flag.Int("healthy-threshold", envflag.Int("LB_HEALTHY_THRESHOLD", 2), "consecutive successful probes before a backend is returned to rotation [env: LB_HEALTHY_THRESHOLD]")

	cpTLSEnable := flag.Bool("control-plane-tls", envflag.Bool("LB_CONTROL_PLANE_TLS", false), "connect to the control plane over TLS [env: LB_CONTROL_PLANE_TLS]")
	cpTLSCACert := flag.String("control-plane-tls-ca", envflag.String("LB_CONTROL_PLANE_TLS_CA", ""), "CA cert to verify the control plane's TLS certificate against; if unset, the system root CA pool is used [env: LB_CONTROL_PLANE_TLS_CA]")
	cpTLSClientCert := flag.String("control-plane-tls-client-cert", envflag.String("LB_CONTROL_PLANE_TLS_CLIENT_CERT", ""), "client cert to present to the control plane (mutual TLS); requires -control-plane-tls-client-key [env: LB_CONTROL_PLANE_TLS_CLIENT_CERT]")
	cpTLSClientKey := flag.String("control-plane-tls-client-key", envflag.String("LB_CONTROL_PLANE_TLS_CLIENT_KEY", ""), "client key matching -control-plane-tls-client-cert [env: LB_CONTROL_PLANE_TLS_CLIENT_KEY]")

	httpTLSCert := flag.String("http-tls-cert", envflag.String("LB_HTTP_TLS_CERT", ""), "path to a TLS certificate for the data plane's HTTP listener; if unset, the listener runs in plaintext HTTP [env: LB_HTTP_TLS_CERT]")
	httpTLSKey := flag.String("http-tls-key", envflag.String("LB_HTTP_TLS_KEY", ""), "path to the TLS private key matching -http-tls-cert [env: LB_HTTP_TLS_KEY]")

	healthReportInterval := flag.Duration("health-report-interval", envflag.Duration("LB_HEALTH_REPORT_INTERVAL", 10*time.Second), "how often to report backend health status back to the control plane, for display in the admin web UI [env: LB_HEALTH_REPORT_INTERVAL]")
	flag.Parse()

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
			log.Fatalf("dataplane: %v", err)
		}
		cpTLSConfig = cfg
	} else {
		log.Println("dataplane: WARNING connecting to control plane without TLS — use -control-plane-tls outside of local development.")
	}

	backends := dataplane.NewBackendList()
	sub := dataplane.NewSubscriber(*controlPlaneAddr, *group, id, backends, cpTLSConfig)
	go sub.Run(ctx)

	healthChecker := dataplane.NewHealthChecker(backends)
	healthChecker.Interval = *healthCheckInterval
	healthChecker.Timeout = *healthCheckTimeout
	healthChecker.FailureThreshold = *unhealthyThreshold
	healthChecker.SuccessThreshold = *healthyThreshold
	go healthChecker.Run(ctx)

	healthReporter := dataplane.NewHealthReporter(*controlPlaneAddr, *group, id, backends, cpTLSConfig, *healthReportInterval)
	go healthReporter.Run(ctx)

	proxy := dataplane.NewProxy(backends)

	mux := http.NewServeMux()
	mux.Handle("/", proxy.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Printf("dataplane: failed to write /healthz response: %v", err)
		}
	})
	mux.HandleFunc("/debug/backends", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		body := "group: " + *group + "\n" +
			"backend count: " + strconv.Itoa(backends.Len()) + "\n" +
			"healthy count: " + strconv.Itoa(backends.HealthyLen()) + "\n" +
			"version: " + strconv.FormatInt(backends.Version(), 10) + "\n"
		if _, err := w.Write([]byte(body)); err != nil {
			log.Printf("dataplane: failed to write /debug/backends response: %v", err)
		}
	})

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Println("dataplane: shutting down HTTP server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("dataplane: instance %q serving group %q on %s, control plane at %s", id, *group, *listenAddr, *controlPlaneAddr)

	var serveErr error
	if *httpTLSCert != "" {
		log.Println("dataplane: HTTP listener TLS enabled")
		serveErr = server.ListenAndServeTLS(*httpTLSCert, *httpTLSKey)
	} else {
		log.Println("dataplane: WARNING HTTP listener running without TLS — use -http-tls-cert/-http-tls-key outside of local development.")
		serveErr = server.ListenAndServe()
	}
	if serveErr != nil && serveErr != http.ErrServerClosed {
		log.Fatalf("dataplane: HTTP server error: %v", serveErr)
	}
}
