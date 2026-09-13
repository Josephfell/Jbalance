// Package grpcauth provides application-layer bearer-token authentication
// for the control-plane gRPC API, independent of the transport-level
// TLS/mTLS the server may also use.
//
// The control plane installs the server-side UnaryInterceptor and
// StreamInterceptor; every data-plane client attaches the matching token
// via TokenCredentials. When the configured token is empty the
// interceptors are pass-through, so authentication is opt-in and existing
// deployments keep working unchanged.
package grpcauth

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// metadataKey is the gRPC metadata key carrying the bearer token. gRPC
// lowercases all metadata keys, so this must be lowercase.
const metadataKey = "authorization"

// bearerPrefix is the scheme prefix on the authorization metadata value.
const bearerPrefix = "Bearer "

// TokenCredentials is a grpc.PerRPCCredentials that attaches a static
// bearer token to every outgoing RPC. RequireTransportSecurity reports
// false so the credential also works over a plaintext connection — but on
// a plaintext connection the token travels in the clear, so pair it with
// transport TLS (-control-plane-tls) in any real deployment.
type TokenCredentials struct {
	token string
}

// NewTokenCredentials returns a PerRPCCredentials attaching the given
// bearer token. The token must be non-empty; callers should only install
// these credentials when a token is configured.
func NewTokenCredentials(token string) TokenCredentials {
	return TokenCredentials{token: token}
}

// GetRequestMetadata implements credentials.PerRPCCredentials.
func (c TokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{metadataKey: bearerPrefix + c.token}, nil
}

// RequireTransportSecurity implements credentials.PerRPCCredentials. It
// returns false so the token can be sent over plaintext for local
// development; production deployments should enable transport TLS.
func (c TokenCredentials) RequireTransportSecurity() bool { return false }

// tokenFromContext extracts the bearer token from the incoming gRPC
// metadata, or "" if absent/malformed.
func tokenFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(metadataKey)
	if len(vals) == 0 {
		return ""
	}
	// A well-formed value is "Bearer <token>". Tolerate a case-insensitive
	// scheme; reject anything without the prefix.
	v := vals[0]
	if len(v) < len(bearerPrefix) || !strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return v[len(bearerPrefix):]
}

// authorize compares the presented token against the expected one in
// constant time. It returns nil when expected is empty (auth disabled) or
// when the presented token matches.
func authorize(ctx context.Context, expected string) error {
	if expected == "" {
		return nil // authentication disabled — pass through
	}
	presented := tokenFromContext(ctx)
	if presented == "" {
		return status.Error(codes.Unauthenticated, "missing bearer token")
	}
	// ConstantTimeCompare returns 1 only when the byte slices are equal AND
	// the same length, so it does not leak length via early return here.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	return nil
}

// UnaryInterceptor returns a server-side unary interceptor enforcing the
// bearer token. When expected is empty it is a pass-through.
func UnaryInterceptor(expected string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(ctx, expected); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns a server-side stream interceptor enforcing the
// bearer token. When expected is empty it is a pass-through.
func StreamInterceptor(expected string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(ss.Context(), expected); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
