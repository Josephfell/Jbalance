// Command controlplane runs the load balancer control plane: it watches a
// backend pool provider (a fake/simulated one by default, for local
// testing) and streams backend list updates to any connected data plane
// instances over gRPC.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/josephfell/go-loadbalancer/internal/admin"
	"github.com/josephfell/go-loadbalancer/internal/controlplane"
	"github.com/josephfell/go-loadbalancer/internal/envflag"
	"github.com/josephfell/go-loadbalancer/internal/pool"
	"github.com/josephfell/go-loadbalancer/internal/tlsutil"
	pb "github.com/josephfell/go-loadbalancer/proto"
)

// Every flag below can also be set via the environment variable named in
// its usage string (e.g. from a Docker Compose env_file) — an explicitly
// passed flag always takes precedence over the environment.
func main() {
	grpcAddr := flag.String("grpc-addr", envflag.String("LB_GRPC_ADDR", ":9090"), "address for the control plane gRPC server to listen on [env: LB_GRPC_ADDR]")
	reconcileInterval := flag.Duration("reconcile-interval", envflag.Duration("LB_RECONCILE_INTERVAL", 2*time.Second), "how often to poll the backend pool provider for changes [env: LB_RECONCILE_INTERVAL]")
	tlsCertFile := flag.String("tls-cert", envflag.String("LB_TLS_CERT", ""), "path to a TLS certificate file for the gRPC server; if unset, the server runs in plaintext [env: LB_TLS_CERT]")
	tlsKeyFile := flag.String("tls-key", envflag.String("LB_TLS_KEY", ""), "path to the TLS private key matching -tls-cert [env: LB_TLS_KEY]")
	tlsClientCAFile := flag.String("tls-client-ca", envflag.String("LB_TLS_CLIENT_CA", ""), "path to a CA cert used to require and verify data plane client certificates (mutual TLS); if unset, any client can connect once TLS is enabled [env: LB_TLS_CLIENT_CA]")

	providerKind := flag.String("provider", envflag.String("LB_PROVIDER", "fake"), "backend pool provider to use: 'fake' (local testing) or 'azure-vmss' [env: LB_PROVIDER]")

	// Fake provider settings.
	simulateScaling := flag.Bool("simulate-scaling", envflag.Bool("LB_SIMULATE_SCALING", true), "(fake provider only) randomly add/remove backends on a timer to simulate scaling events [env: LB_SIMULATE_SCALING]")
	scalingInterval := flag.Duration("scaling-interval", envflag.Duration("LB_SCALING_INTERVAL", 5*time.Second), "(fake provider only) how often to simulate a scaling event [env: LB_SCALING_INTERVAL]")
	fakeBasePort := flag.Int("fake-base-port", envflag.Int("LB_FAKE_BASE_PORT", 8081), "(fake provider only) starting port number for fake backend addresses [env: LB_FAKE_BASE_PORT]")

	// Azure VMSS provider settings.
	azureSubscriptionID := flag.String("azure-subscription-id", envflag.String("LB_AZURE_SUBSCRIPTION_ID", ""), "(azure-vmss provider only) Azure subscription ID [env: LB_AZURE_SUBSCRIPTION_ID]")
	azureResourceGroup := flag.String("azure-resource-group", envflag.String("LB_AZURE_RESOURCE_GROUP", ""), "(azure-vmss provider only) resource group containing the scale set(s) [env: LB_AZURE_RESOURCE_GROUP]")
	azureVMSSGroups := flag.String("azure-vmss-groups", envflag.String("LB_AZURE_VMSS_GROUPS", ""), "(azure-vmss provider only) comma-separated group specs: group:scaleSetName:port[:weight], e.g. 'web-tier:vmss-web:8080,api-tier:vmss-api:8081' [env: LB_AZURE_VMSS_GROUPS]")

	// Admin web UI settings.
	adminAddr := flag.String("admin-addr", envflag.String("LB_ADMIN_ADDR", ":9091"), "address for the admin web management UI to listen on [env: LB_ADMIN_ADDR]")
	adminDisable := flag.Bool("admin-disable", envflag.Bool("LB_ADMIN_DISABLE", false), "disable the admin web management UI entirely [env: LB_ADMIN_DISABLE]")
	adminStorePath := flag.String("admin-store-path", envflag.String("LB_ADMIN_STORE_PATH", "/var/lib/go-loadbalancer/admin.json"), "path to the local admin store file (password hash + session secret) [env: LB_ADMIN_STORE_PATH]")
	adminAuditLogPath := flag.String("admin-audit-log-path", envflag.String("LB_ADMIN_AUDIT_LOG_PATH", "/var/lib/go-loadbalancer/audit.json"), "path to the local admin audit log file [env: LB_ADMIN_AUDIT_LOG_PATH]")
	adminOverridesPath := flag.String("admin-overrides-path", envflag.String("LB_ADMIN_OVERRIDES_PATH", "/var/lib/go-loadbalancer/overrides.json"), "path to the local file storing manual per-backend weight/drain overrides set via the admin web UI [env: LB_ADMIN_OVERRIDES_PATH]")
	adminAlgorithmsPath := flag.String("admin-algorithms-path", envflag.String("LB_ADMIN_ALGORITHMS_PATH", "/var/lib/go-loadbalancer/algorithms.json"), "path to the local file storing per-group load-balancing algorithm selections set via the admin web UI [env: LB_ADMIN_ALGORITHMS_PATH]")
	adminTLSCert := flag.String("admin-tls-cert", envflag.String("LB_ADMIN_TLS_CERT", ""), "path to a TLS certificate for the admin web UI; if unset, it serves plain HTTP [env: LB_ADMIN_TLS_CERT]")
	adminTLSKey := flag.String("admin-tls-key", envflag.String("LB_ADMIN_TLS_KEY", ""), "path to the TLS private key matching -admin-tls-cert [env: LB_ADMIN_TLS_KEY]")
	adminTrustForwardedFor := flag.Bool("admin-trust-forwarded-for", envflag.Bool("LB_ADMIN_TRUST_FORWARDED_FOR", false), "trust the X-Forwarded-For header for admin login rate limiting; only enable behind a trusted reverse proxy [env: LB_ADMIN_TRUST_FORWARDED_FOR]")
	adminForceResetPassword := flag.Bool("admin-force-reset-password", envflag.Bool("LB_ADMIN_FORCE_RESET_PASSWORD", false), "generate a new random admin password on startup, printed to the log, even if a password is already set — use this to recover from a lost password [env: LB_ADMIN_FORCE_RESET_PASSWORD]")

	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	provider, cleanup, err := buildProvider(ctx, *providerKind, providerConfig{
		fakeBasePort:    *fakeBasePort,
		simulateScaling: *simulateScaling,
		scalingInterval: *scalingInterval,

		azureSubscriptionID: *azureSubscriptionID,
		azureResourceGroup:  *azureResourceGroup,
		azureVMSSGroups:     *azureVMSSGroups,
	})
	if err != nil {
		log.Fatalf("controlplane: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	overrides := controlplane.NewOverrideStore(*adminOverridesPath)
	algorithms := controlplane.NewAlgorithmStore(*adminAlgorithmsPath)
	srv := controlplane.NewServer(provider, overrides, algorithms)
	go srv.Run(ctx, *reconcileInterval)

	if !*adminDisable {
		if err := startAdminServer(ctx, srv, adminConfig{
			addr:              *adminAddr,
			storePath:         *adminStorePath,
			auditLogPath:      *adminAuditLogPath,
			tlsCert:           *adminTLSCert,
			tlsKey:            *adminTLSKey,
			trustForwardedFor: *adminTrustForwardedFor,
			forceReset:        *adminForceResetPassword,
		}); err != nil {
			log.Fatalf("controlplane: failed to start admin web UI: %v", err)
		}
	} else {
		log.Println("controlplane: admin web UI disabled (-admin-disable)")
	}

	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("controlplane: failed to listen on %s: %v", *grpcAddr, err)
	}

	var serverOpts []grpc.ServerOption
	if *tlsCertFile != "" {
		tlsConfig, err := tlsutil.LoadServerConfig(*tlsCertFile, *tlsKeyFile, *tlsClientCAFile)
		if err != nil {
			log.Fatalf("controlplane: %v", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		log.Printf("controlplane: TLS enabled (mutual TLS: %v)", *tlsClientCAFile != "")
	} else {
		log.Println("controlplane: WARNING running without TLS — gRPC traffic is unencrypted. Use -tls-cert/-tls-key outside of local development.")
	}

	grpcServer := grpc.NewServer(serverOpts...)
	pb.RegisterControlPlaneServer(grpcServer, srv)

	go func() {
		<-ctx.Done()
		log.Println("controlplane: shutting down gRPC server...")
		grpcServer.GracefulStop()
	}()

	log.Printf("controlplane: gRPC server listening on %s", *grpcAddr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("controlplane: gRPC server error: %v", err)
	}
}

type adminConfig struct {
	addr              string
	storePath         string
	auditLogPath      string
	tlsCert           string
	tlsKey            string
	trustForwardedFor bool
	forceReset        bool
}

// startAdminServer opens the local admin store (generating a random
// password on first run, printed once to the log), optionally force-resets
// the password, and starts the admin web UI in the background.
func startAdminServer(ctx context.Context, cpServer *controlplane.Server, cfg adminConfig) error {
	store, generated, err := admin.Open(cfg.storePath)
	if err != nil {
		return err
	}

	auditLog := admin.OpenAuditLog(cfg.auditLogPath)

	if generated != nil {
		log.Println("=========================================================")
		log.Println(" Go Load Balancer — admin web UI initial password")
		log.Println("")
		log.Printf("   Password: %s", generated.Password)
		log.Println("")
		log.Println(" This password is shown ONLY ONCE and is not stored in")
		log.Println(" plaintext anywhere. Save it now. You can change it from")
		log.Println(" the web UI after signing in, or recover a lost password")
		log.Println(" by restarting with -admin-force-reset-password.")
		log.Println("=========================================================")
	} else if cfg.forceReset {
		newPassword, err := store.ResetToRandomPassword()
		if err != nil {
			return fmt.Errorf("failed to force-reset admin password: %w", err)
		}
		auditLog.Record(admin.AuditPasswordReset, "", "password force-reset via -admin-force-reset-password")
		log.Println("=========================================================")
		log.Println(" Go Load Balancer — admin password RESET (-admin-force-reset-password)")
		log.Println("")
		log.Printf("   New password: %s", newPassword)
		log.Println("")
		log.Println(" All existing admin sessions have been signed out.")
		log.Println("=========================================================")
	}

	adminSrv, err := admin.NewServer(store, cpServer, auditLog, cfg.tlsCert != "", cfg.trustForwardedFor)
	if err != nil {
		return fmt.Errorf("failed to create admin server: %w", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           adminSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// Deliberately use a fresh context here rather than the
		// already-cancelled ctx — Shutdown needs its own timeout budget to
		// drain in-flight requests, independent of why the parent context
		// was cancelled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	go func() {
		var serveErr error
		if cfg.tlsCert != "" {
			log.Printf("controlplane: admin web UI listening on %s (TLS enabled)", cfg.addr)
			serveErr = httpServer.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			log.Printf("controlplane: WARNING admin web UI listening on %s without TLS — use -admin-tls-cert/-admin-tls-key outside of local development.", cfg.addr)
			serveErr = httpServer.ListenAndServe()
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Printf("controlplane: admin web UI server error: %v", serveErr)
		}
	}()

	return nil
}

type providerConfig struct {
	fakeBasePort    int
	simulateScaling bool
	scalingInterval time.Duration

	azureSubscriptionID string
	azureResourceGroup  string
	azureVMSSGroups     string
}

// buildProvider constructs the configured pool.Provider. The returned
// cleanup func (may be nil) should be called on shutdown.
func buildProvider(ctx context.Context, kind string, cfg providerConfig) (pool.Provider, func(), error) {
	switch kind {
	case "fake":
		provider := pool.NewFakeProvider(cfg.fakeBasePort, map[string]int{
			"web-tier": 3,
			"api-tier": 2,
		})
		if cfg.simulateScaling {
			go provider.SimulateScaling(ctx, cfg.scalingInterval, 1, 6)
			log.Printf("controlplane: simulating scaling events every %s", cfg.scalingInterval)
		}
		return provider, nil, nil

	case "azure-vmss":
		if cfg.azureSubscriptionID == "" {
			return nil, nil, fmt.Errorf("-azure-subscription-id (or LB_AZURE_SUBSCRIPTION_ID) is required for the azure-vmss provider")
		}
		if cfg.azureResourceGroup == "" {
			return nil, nil, fmt.Errorf("-azure-resource-group (or LB_AZURE_RESOURCE_GROUP) is required for the azure-vmss provider")
		}
		groups, err := parseAzureVMSSGroups(cfg.azureVMSSGroups)
		if err != nil {
			return nil, nil, err
		}
		if len(groups) == 0 {
			return nil, nil, fmt.Errorf("-azure-vmss-groups (or LB_AZURE_VMSS_GROUPS) must specify at least one group")
		}

		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create Azure credential: %w", err)
		}

		provider, err := pool.NewAzureVMSSProvider(cfg.azureSubscriptionID, cfg.azureResourceGroup, groups, cred)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create azure-vmss provider: %w", err)
		}
		log.Printf("controlplane: using azure-vmss provider for %d group(s) in resource group %q", len(groups), cfg.azureResourceGroup)
		return provider, nil, nil

	default:
		return nil, nil, fmt.Errorf("unknown provider %q (must be 'fake' or 'azure-vmss')", kind)
	}
}

// parseAzureVMSSGroups parses a comma-separated list of
// "group:scaleSetName:port[:weight]" specs into AzureVMSSGroup values.
func parseAzureVMSSGroups(spec string) ([]pool.AzureVMSSGroup, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	var groups []pool.AzureVMSSGroup
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 3 || len(parts) > 4 {
			return nil, fmt.Errorf("invalid azure-vmss-groups entry %q: expected group:scaleSetName:port[:weight]", entry)
		}

		portVal, err := strconv.ParseInt(parts[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid port in azure-vmss-groups entry %q: %w", entry, err)
		}
		port := int(portVal)

		var weight int32 = 1
		if len(parts) == 4 {
			w, err := strconv.ParseInt(parts[3], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid weight in azure-vmss-groups entry %q: %w", entry, err)
			}
			weight = int32(w)
		}

		groups = append(groups, pool.AzureVMSSGroup{
			Group:        parts[0],
			ScaleSetName: parts[1],
			Port:         port,
			Weight:       weight,
		})
	}

	return groups, nil
}
