package dataplane

import (
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/Josephfell/Jbalance/internal/grpcauth"
)

// dialOptions builds the gRPC dial options shared by every control-plane
// client (subscriber, route subscriber, health reporter, metrics
// reporter): transport credentials from tlsConfig (nil = plaintext) and,
// when authToken is non-empty, per-RPC bearer-token credentials so the
// control plane's auth interceptors admit the connection.
func dialOptions(tlsConfig *tls.Config, authToken string) []grpc.DialOption {
	var transportCreds credentials.TransportCredentials
	if tlsConfig != nil {
		transportCreds = credentials.NewTLS(tlsConfig)
	} else {
		transportCreds = insecure.NewCredentials()
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(transportCreds)}
	if authToken != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(grpcauth.NewTokenCredentials(authToken)))
	}
	return opts
}
