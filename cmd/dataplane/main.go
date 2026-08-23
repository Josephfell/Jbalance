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
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	protocol := flag.String("protocol", envflag.String("LB_PROTOCOL", "http"), "which proxy mode to run: 'http' (L7, supports routing and sticky sessions) or 'tcp' (L4, raw byte forwarding, single backend group) [env: LB_PROTOCOL]")
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

	if *protocol != "http" && *protocol != "tcp" {
		log.Fatalf("dataplane: unknown -protocol %q (must be 'http' or 'tcp')", *protocol)
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
	}, *healthReportInterval)
	defaultBackends := groups.Ensure(*group)

	if *protocol == "tcp" {
		runTCP(ctx, id, *group, *listenAddr, *controlPlaneAddr, defaultBackends, *httpTLSCert, *httpTLSKey)
		return
	}

	routes := dataplane.NewRouteTable(*group)
	routeSub := dataplane.NewRouteSubscriber(*controlPlaneAddr, id, routes, cpTLSConfig)
	go routeSub.Run(ctx)

	proxy := dataplane.NewProxy(routes, groups)

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
		body := "default group: " + *group + "\n" +
			"backend count: " + strconv.Itoa(defaultBackends.Len()) + "\n" +
			"healthy count: " + strconv.Itoa(defaultBackends.HealthyLen()) + "\n" +
			"version: " + strconv.FormatInt(defaultBackends.Version(), 10) + "\n" +
			"routes configured: " + strconv.Itoa(routes.Len()) + "\n" +
			"tracked groups: " + strings.Join(groups.Groups(), ", ") + "\n"
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

// runTCP runs the L4 (raw TCP) proxy loop. Unlike the HTTP path, there is
// no L7 routing (a TCP proxy has no visibility into what's inside the
// bytes it forwards) and no debug/healthz HTTP endpoints — group is the
// only backend group this listener will ever proxy to, for its entire
// lifetime, exactly like an L7 data plane instance before routing existed.
func runTCP(ctx context.Context, id, group, listenAddr, controlPlaneAddr string, backends *dataplane.BackendList, tlsCert, tlsKey string) {
	var ln net.Listener
	var err error
	if tlsCert != "" {
		cert, certErr := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if certErr != nil {
			log.Fatalf("dataplane: failed to load TCP listener TLS cert/key: %v", certErr)
		}
		log.Println("dataplane: TCP listener TLS enabled")
		ln, err = tls.Listen("tcp", listenAddr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	} else {
		log.Println("dataplane: WARNING TCP listener running without TLS — use -http-tls-cert/-http-tls-key outside of local development.")
		ln, err = net.Listen("tcp", listenAddr)
	}
	if err != nil {
		log.Fatalf("dataplane: failed to listen on %s: %v", listenAddr, err)
	}

	go func() {
		<-ctx.Done()
		log.Println("dataplane: shutting down TCP listener...")
		_ = ln.Close()
	}()

	log.Printf("dataplane: instance %q serving group %q (tcp) on %s, control plane at %s", id, group, listenAddr, controlPlaneAddr)

	proxy := dataplane.NewTCPProxy(backends)
	if err := proxy.Serve(ctx, ln); err != nil {
		log.Fatalf("dataplane: TCP proxy error: %v", err)
	}
}
