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

// Subscriber connects to a control plane and keeps a BackendList updated
// with whatever it streams for the configured group. Automatically
// reconnects with backoff if the stream drops.
type Subscriber struct {
	controlPlaneAddr string
	group            string
	instanceID       string
	backends         *BackendList
	tlsConfig        *tls.Config // nil means plaintext
}

// NewSubscriber creates a subscriber that will update the given BackendList
// as updates arrive from the control plane at controlPlaneAddr. Pass a
// non-nil tlsConfig to connect over TLS; pass nil for plaintext (local
// development only).
func NewSubscriber(controlPlaneAddr, group, instanceID string, backends *BackendList, tlsConfig *tls.Config) *Subscriber {
	return &Subscriber{
		controlPlaneAddr: controlPlaneAddr,
		group:            group,
		instanceID:       instanceID,
		backends:         backends,
		tlsConfig:        tlsConfig,
	}
}

// Run connects to the control plane and streams backend updates until ctx
// is cancelled, reconnecting automatically (with capped exponential
// backoff) if the connection drops.
func (s *Subscriber) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		receivedAny, err := s.streamOnce(ctx)
		if receivedAny {
			// We got at least one update before the stream ended, so treat
			// the connection as having been healthy — reset backoff rather
			// than penalising a brief, otherwise-fine disconnect.
			backoff = time.Second
		}

		if err != nil {
			log.Printf("dataplane: control plane stream error: %v (retrying in %s)", err, backoff)
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

		// streamOnce returned cleanly (context cancelled) — nothing more to do.
		return
	}
}

// streamOnce connects and streams until the stream ends or ctx is
// cancelled. Returns whether at least one BackendSet was received, and any
// error encountered (nil if ending was due to context cancellation).
func (s *Subscriber) streamOnce(ctx context.Context) (bool, error) {
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
			log.Printf("dataplane: error closing control plane connection: %v", cerr)
		}
	}()

	client := pb.NewControlPlaneClient(conn)
	stream, err := client.StreamBackends(ctx, &pb.StreamBackendsRequest{
		Group:      s.group,
		InstanceId: s.instanceID,
	})
	if err != nil {
		return false, err
	}

	log.Printf("dataplane: connected to control plane at %s for group %q", s.controlPlaneAddr, s.group)

	receivedAny := false
	for {
		set, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return receivedAny, nil
			}
			return receivedAny, err
		}
		receivedAny = true
		s.backends.Update(set)
		log.Printf("dataplane: group %q updated to version %d (%d backends)", set.Group, set.Version, len(set.Backends))
	}
}
