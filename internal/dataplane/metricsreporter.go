package dataplane

import (
	"context"
	"crypto/tls"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/Josephfell/Jbalance/proto"
)

// MetricsReporter periodically pushes this data plane instance's traffic
// summary (requests, error rate, active connections, average latency —
// per group) to the control plane via the ReportMetrics RPC, mirroring
// HealthReporter's push model. This is how the admin web UI's live
// charts see per-instance traffic without the control plane needing a
// route back to scrape each data plane's own Prometheus /metrics
// endpoint directly (only the reverse connection exists in this
// architecture).
type MetricsReporter struct {
	controlPlaneAddr string
	instanceID       string
	metrics          *Metrics
	tlsConfig        *tls.Config // nil means plaintext
	interval         time.Duration
}

// NewMetricsReporter creates a reporter that sends metrics to the control
// plane at controlPlaneAddr every interval.
func NewMetricsReporter(controlPlaneAddr, instanceID string, metrics *Metrics, tlsConfig *tls.Config, interval time.Duration) *MetricsReporter {
	return &MetricsReporter{
		controlPlaneAddr: controlPlaneAddr,
		instanceID:       instanceID,
		metrics:          metrics,
		tlsConfig:        tlsConfig,
		interval:         interval,
	}
}

// Run connects to the control plane and reports metrics on a timer until
// ctx is cancelled. Connection failures are logged and retried on the
// next tick rather than treated as fatal — reporting metrics is an
// observability nice-to-have, never a reason to stop proxying traffic.
func (r *MetricsReporter) Run(ctx context.Context) {
	var transportCreds credentials.TransportCredentials
	if r.tlsConfig != nil {
		transportCreds = credentials.NewTLS(r.tlsConfig)
	} else {
		transportCreds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(r.controlPlaneAddr, grpc.WithTransportCredentials(transportCreds))
	if err != nil {
		log.Printf("dataplane: metrics reporter failed to create client: %v", err)
		return
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			log.Printf("dataplane: error closing metrics reporter connection: %v", cerr)
		}
	}()

	client := pb.NewControlPlaneClient(conn)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reportOnce(ctx, client)
		}
	}
}

func (r *MetricsReporter) reportOnce(ctx context.Context, client pb.ControlPlaneClient) {
	snapshots := r.metrics.GroupSnapshot()
	if len(snapshots) == 0 {
		return // nothing proxied yet — nothing to report
	}

	groups := make([]*pb.GroupMetrics, 0, len(snapshots))
	for _, s := range snapshots {
		groups = append(groups, &pb.GroupMetrics{
			Group:             s.Group,
			RequestsTotal:     s.RequestsTotal,
			Errors_5XxTotal:   s.Errors5xxTotal,
			ActiveConnections: s.ActiveConns,
			AvgDurationMs:     s.AvgDurationMs,
		})
	}

	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := client.ReportMetrics(reportCtx, &pb.MetricsReport{
		InstanceId: r.instanceID,
		Groups:     groups,
	})
	if err != nil {
		log.Printf("dataplane: failed to report metrics to control plane: %v", err)
	}
}
