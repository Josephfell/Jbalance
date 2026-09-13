package grpcauth

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ctxWithToken builds an incoming-metadata context as the server sees it,
// using the same key/prefix the client credentials produce.
func ctxWithToken(token string) context.Context {
	md := metadata.Pairs(metadataKey, bearerPrefix+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if want == codes.OK {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return
	}
	if status.Code(err) != want {
		t.Fatalf("expected code %v, got %v (err=%v)", want, status.Code(err), err)
	}
}

func TestTokenCredentials(t *testing.T) {
	c := NewTokenCredentials("s3cret")
	md, err := c.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if got := md[metadataKey]; got != "Bearer s3cret" {
		t.Fatalf("metadata value = %q, want %q", got, "Bearer s3cret")
	}
	if c.RequireTransportSecurity() {
		t.Error("RequireTransportSecurity should be false")
	}
}

func TestUnaryInterceptor(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{}

	tests := []struct {
		name        string
		expected    string
		ctx         context.Context
		wantCode    codes.Code
		wantHandler bool
	}{
		{"disabled passes through with no token", "", context.Background(), codes.OK, true},
		{"disabled passes through with a token", "", ctxWithToken("anything"), codes.OK, true},
		{"correct token accepted", "good", ctxWithToken("good"), codes.OK, true},
		{"wrong token rejected", "good", ctxWithToken("bad"), codes.Unauthenticated, false},
		{"missing token rejected", "good", context.Background(), codes.Unauthenticated, false},
		{"empty presented token rejected", "good", ctxWithToken(""), codes.Unauthenticated, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called = false
			interceptor := UnaryInterceptor(tt.expected)
			_, err := interceptor(tt.ctx, nil, info, handler)
			wantCode(t, err, tt.wantCode)
			if called != tt.wantHandler {
				t.Errorf("handler called = %v, want %v", called, tt.wantHandler)
			}
		})
	}
}

// fakeServerStream is a minimal grpc.ServerStream carrying a context.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeServerStream) Context() context.Context { return f.ctx }

func TestStreamInterceptor(t *testing.T) {
	handlerErr := errors.New("handler ran")
	handler := func(srv any, ss grpc.ServerStream) error { return handlerErr }
	info := &grpc.StreamServerInfo{}

	tests := []struct {
		name       string
		expected   string
		ctx        context.Context
		wantCode   codes.Code
		wantRanHdl bool
	}{
		{"disabled passes through", "", context.Background(), codes.Unknown, true},
		{"correct token accepted", "good", ctxWithToken("good"), codes.Unknown, true},
		{"wrong token rejected", "good", ctxWithToken("bad"), codes.Unauthenticated, false},
		{"missing token rejected", "good", context.Background(), codes.Unauthenticated, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interceptor := StreamInterceptor(tt.expected)
			err := interceptor(nil, fakeServerStream{ctx: tt.ctx}, info, handler)
			if tt.wantRanHdl {
				// Pass-through: our fake handler returns handlerErr, which is
				// a plain error (code Unknown), proving the handler ran.
				if !errors.Is(err, handlerErr) {
					t.Fatalf("expected handler to run (err=%v)", err)
				}
				return
			}
			wantCode(t, err, tt.wantCode)
		})
	}
}
