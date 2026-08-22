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

// RouteSubscriber connects to a control plane and keeps a RouteTable
// updated with whatever L7 route table it streams. Structurally identical
// to Subscriber (same reconnect/backoff behaviour) but against the
// StreamRoutes RPC instead of StreamBackends, and not scoped to a group —
// the route table is global.
type RouteSubscriber struct {
	controlPlaneAddr string
	instanceID       string
	routes           *RouteTable
	tlsConfig        *tls.Config // nil means plaintext
}

// NewRouteSubscriber creates a subscriber that will update the given
// RouteTable as updates arrive from the control plane at
// controlPlaneAddr.
func NewRouteSubscriber(controlPlaneAddr, instanceID string, routes *RouteTable, tlsConfig *tls.Config) *RouteSubscriber {
	return &RouteSubscriber{
		controlPlaneAddr: controlPlaneAddr,
		instanceID:       instanceID,
		routes:           routes,
		tlsConfig:        tlsConfig,
	}
}

// Run connects to the control plane and streams route table updates until
// ctx is cancelled, reconnecting automatically (with capped exponential
// backoff) if the connection drops. Mirrors Subscriber.Run.
func (s *RouteSubscriber) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		receivedAny, err := s.streamOnce(ctx)
		if receivedAny {
			backoff = time.Second
		}

		if err != nil {
			log.Printf("dataplane: route table stream error: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		return
	}
}

func (s *RouteSubscriber) streamOnce(ctx context.Context) (bool, error) {
	var transportCreds credentials.TransportCredentials
	if s.tlsConfig != nil {
		transportCreds = credentials.NewTLS(s.tlsConfig)
	} else {
		transportCreds = insecure.NewCredentials()
	}

	conn, err := grpc.NewClient(s.controlPlaneAddr, grpc.WithTransportCredentials(transportCreds))
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			log.Printf("dataplane: error closing route table connection: %v", cerr)
		}
	}()

	client := pb.NewControlPlaneClient(conn)
	stream, err := client.StreamRoutes(ctx, &pb.StreamRoutesRequest{InstanceId: s.instanceID})
	if err != nil {
		return false, err
	}

	log.Printf("dataplane: connected to control plane at %s for route table", s.controlPlaneAddr)

	receivedAny := false
	for {
		table, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return receivedAny, nil
			}
			return receivedAny, err
		}
		receivedAny = true
		s.routes.Update(table)
		log.Printf("dataplane: route table updated to version %d (%d rules)", table.Version, len(table.Routes))
	}
}
