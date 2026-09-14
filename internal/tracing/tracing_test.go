package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestSetupDisabledIsNoOp(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup(disabled): %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func should never be nil")
	}
	// Safe to call.
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown(disabled): %v", err)
	}
	// Even when disabled, the W3C propagator is installed so traceparent is
	// honoured/emitted consistently.
	fields := otel.GetTextMapPropagator().Fields()
	found := false
	for _, f := range fields {
		if f == "traceparent" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the W3C traceparent propagator to be installed, fields=%v", fields)
	}
}

func TestSetupUnknownProtocol(t *testing.T) {
	_, err := Setup(context.Background(), Config{Enabled: true, Protocol: "carrier-pigeon", Endpoint: "localhost:4317", Insecure: true})
	if err == nil {
		t.Fatal("expected an error for an unknown OTLP protocol")
	}
}

func TestServiceNameDefault(t *testing.T) {
	if got := serviceName(""); got != "jbalance-dataplane" {
		t.Errorf("serviceName(\"\") = %q, want jbalance-dataplane", got)
	}
	if got := serviceName("custom"); got != "custom" {
		t.Errorf("serviceName(custom) = %q, want custom", got)
	}
}
