// Package tracing wires OpenTelemetry distributed tracing into the data
// plane's L7 request path. It is opt-in: when disabled (the default), Setup
// installs a no-op tracer provider and the middleware adds negligible
// overhead, so a deployment that never enables tracing is unchanged.
//
// When enabled, each proxied request becomes a span, inbound W3C
// traceparent headers are honoured (so a trace started at an upstream
// edge/CDN continues through this hop), and the context is propagated to
// the backend — letting a request be followed client -> proxy -> backend in
// Jaeger/Tempo/Datadog/etc. The existing X-Request-Id correlation is
// unaffected and complementary.
package tracing

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config controls tracing setup. Zero value (Enabled=false) is a no-op.
type Config struct {
	// Enabled turns tracing on. When false, Setup installs a no-op
	// provider and returns a nil-safe shutdown func.
	Enabled bool
	// Endpoint is the OTLP collector address (host:port), e.g.
	// "localhost:4317" for gRPC or "localhost:4318" for HTTP.
	Endpoint string
	// Protocol selects the OTLP transport: "grpc" (default) or "http".
	Protocol string
	// Insecure disables transport TLS to the collector (plaintext OTLP).
	Insecure bool
	// ServiceName is reported as the service.name resource attribute.
	ServiceName string
	// SampleRatio is the head-based sampling ratio in [0,1]. 1 samples
	// every trace (the default when unset/<=0 with Enabled), 0 samples
	// none.
	SampleRatio float64
}

// Setup configures the global OpenTelemetry tracer provider and text-map
// propagator from cfg. It returns a shutdown function that flushes and
// stops the exporter; it is always safe to call (a no-op when tracing is
// disabled). Setup is called once at process startup.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	// Always install the W3C propagators so traceparent is honoured/emitted
	// consistently; harmless when tracing is disabled.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName(cfg.ServiceName))),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)

	return func(shutdownCtx context.Context) error {
		ctx, cancel := context.WithTimeout(shutdownCtx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Protocol {
	case "", "grpc":
		opts := []otlptracegrpc.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	case "http":
		opts := []otlptracehttp.Option{}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unknown OTLP protocol %q (want grpc or http)", cfg.Protocol)
	}
}

func serviceName(n string) string {
	if n == "" {
		return "jbalance-dataplane"
	}
	return n
}

// Middleware wraps an http.Handler so each request is traced: it starts
// (or continues, from an inbound traceparent) a span named by the route,
// and makes the span context available to inner handlers and the backend.
// When tracing is disabled the global provider is a no-op, so this adds
// negligible overhead and can be installed unconditionally.
func Middleware(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "jbalance.proxy")
}
