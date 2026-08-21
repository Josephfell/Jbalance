package dataplane

import (
	"context"
	"crypto/tls"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/josephfell/go-loadbalancer/proto"
)

// HealthReporter periodically sends this data plane instance's locally
// observed backend health (as tracked in a BackendList, updated by a
// HealthChecker) to the control plane via the ReportHealth RPC. This is
// how the control plane — which has no direct visibility into backend
// health on its own — learns what each data plane believes about the
// backends it's actually proxying to, for display in the admin web UI.
//
// Uses a separate gRPC connection from Subscriber's streaming connection,
// since ReportHealth is a plain unary call on its own schedule and
// shouldn't be coupled to the backend-list stream's lifecycle.
type HealthReporter struct {
	controlPlaneAddr string
	group            string
	instanceID       string
	backends         *BackendList
	tlsConfig        *tls.Config // nil means plaintext
	interval         time.Duration
}

// NewHealthReporter creates a reporter that sends backends' health status
// to the control plane at controlPlaneAddr every interval.
func NewHealthReporter(controlPlaneAddr, group, instanceID string, backends *BackendList, tlsConfig *tls.Config, interval time.Duration) *HealthReporter {
	return &HealthReporter{
		controlPlaneAddr: controlPlaneAddr,
		group:            group,
		instanceID:       instanceID,
		backends:         backends,
		tlsConfig:        tlsConfig,
		interval:         interval,
	}
}

// Run connects to the control plane and reports health on a timer until
// ctx is cancelled. Connection failures are logged and retried on the next
// tick rather than treated as fatal — a data plane should keep proxying
// traffic even if health reporting is temporarily unable to reach the
// control plane.
func (r *HealthReporter) Run(ctx context.Context) {
	var transportCreds credentials.TransportCredentials
	if r.tlsConfig != nil {
		transportCreds = credentials.NewTLS(r.tlsConfig)
	} else {
		transportCreds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(r.controlPlaneAddr, grpc.WithTransportCredentials(transportCreds))
	if err != nil {
		log.Printf("dataplane: health reporter failed to create client: %v", err)
		return
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			log.Printf("dataplane: error closing health reporter connection: %v", cerr)
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

func (r *HealthReporter) reportOnce(ctx context.Context, client pb.ControlPlaneClient) {
	snapshot := r.backends.HealthSnapshot()
	if len(snapshot) == 0 {
		return // nothing to report yet
	}

	backends := make([]*pb.BackendHealth, 0, len(snapshot))
	for _, s := range snapshot {
		backends = append(backends, &pb.BackendHealth{Address: s.Address, Healthy: s.Healthy})
	}

	reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := client.ReportHealth(reportCtx, &pb.HealthReport{
		Group:      r.group,
		InstanceId: r.instanceID,
		Backends:   backends,
	})
	if err != nil {
		log.Printf("dataplane: failed to report health to control plane: %v", err)
	}
}
