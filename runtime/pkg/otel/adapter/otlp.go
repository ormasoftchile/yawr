package adapter

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	otelPkg "github.com/ormasoftchile/yawr/runtime/pkg/otel"
)

// config holds options for the OTLP TracerProvider.
type config struct {
	serviceName string
	headers     map[string]string
	insecure    bool
	tlsConfig   *tls.Config
}

// Option configures an OTLP TracerProvider.
type Option func(*config)

// WithServiceName sets the service.name resource attribute.
func WithServiceName(name string) Option {
	return func(c *config) { c.serviceName = name }
}

// WithHeaders adds metadata headers to OTLP gRPC requests.
func WithHeaders(headers map[string]string) Option {
	return func(c *config) {
		if c.headers == nil {
			c.headers = make(map[string]string)
		}
		for k, v := range headers {
			c.headers[k] = v
		}
	}
}

// WithInsecure allows non-TLS connections (local collectors, testing).
func WithInsecure() Option {
	return func(c *config) { c.insecure = true }
}

// WithTLS enables TLS with the given config. If tlsCfg is nil, the system
// default TLS configuration is used (verifies server cert via OS trust store).
func WithTLS(tlsCfg *tls.Config) Option {
	return func(c *config) {
		c.insecure = false
		c.tlsConfig = tlsCfg
	}
}

// NewOTLPTracerProvider creates a [otelPkg.TracerProvider] that exports spans
// via OTLP gRPC to the given endpoint (e.g. "localhost:4317").
//
// The second return value is a shutdown func that flushes pending spans and
// releases resources; always call it on process exit.
func NewOTLPTracerProvider(endpoint string, opts ...Option) (otelPkg.TracerProvider, func(), error) {
	if endpoint == "" {
		return nil, nil, fmt.Errorf("adapter: OTLP endpoint must not be empty")
	}

	cfg := &config{
		serviceName: "yawr",
		insecure:    true, // default to insecure for local / dev use
	}
	for _, o := range opts {
		o(cfg)
	}

	dialOpts := []grpc.DialOption{}
	if cfg.insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		creds := credentials.NewTLS(cfg.tlsConfig) // nil → system default
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
	}

	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithDialOption(dialOpts...),
	}
	if len(cfg.headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithHeaders(cfg.headers))
	}

	ctx := context.Background()
	exp, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("adapter: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", cfg.serviceName),
		),
	)
	if err != nil {
		_ = exp.Shutdown(context.Background())
		return nil, nil, fmt.Errorf("adapter: create OTel resource: %w", err)
	}

	sdkProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sdkProvider.Shutdown(shutdownCtx)
	}

	return &sdkTracerProviderAdapter{provider: sdkProvider}, shutdown, nil
}

// sdkTracerProviderAdapter wraps the OTel SDK TracerProvider to implement yawr's otelPkg.TracerProvider.
type sdkTracerProviderAdapter struct {
	provider *sdktrace.TracerProvider
}

func (a *sdkTracerProviderAdapter) Tracer(name string, _ ...otelPkg.TracerOption) otelPkg.Tracer {
	return &sdkTracerAdapter{tracer: a.provider.Tracer(name)}
}

// sdkTracerAdapter wraps an OTel SDK Tracer to implement yawr's otelPkg.Tracer.
type sdkTracerAdapter struct {
	tracer oteltrace.Tracer
}

func (t *sdkTracerAdapter) Start(ctx context.Context, spanName string, opts ...otelPkg.SpanStartOption) (context.Context, otelPkg.Span) {
	ctx, span := t.tracer.Start(ctx, spanName)
	return ctx, &sdkSpanAdapter{span: span}
}

// sdkSpanAdapter wraps an OTel SDK Span to implement yawr's otelPkg.Span.
type sdkSpanAdapter struct {
	span oteltrace.Span
}

func (s *sdkSpanAdapter) End(_ ...otelPkg.SpanEndOption) {
	s.span.End()
}

func (s *sdkSpanAdapter) SetAttributes(kv ...otelPkg.Attribute) {
	attrs := make([]attribute.KeyValue, 0, len(kv))
	for _, a := range kv {
		attrs = append(attrs, attribute.String(a.Key, fmt.Sprint(a.Value)))
	}
	s.span.SetAttributes(attrs...)
}

func (s *sdkSpanAdapter) RecordError(err error, _ ...otelPkg.EventOption) {
	s.span.RecordError(err)
}

func (s *sdkSpanAdapter) SetStatus(code otelPkg.StatusCode, description string) {
	switch code {
	case otelPkg.StatusOK:
		s.span.SetStatus(codes.Ok, description)
	case otelPkg.StatusError:
		s.span.SetStatus(codes.Error, description)
	default:
		s.span.SetStatus(codes.Unset, description)
	}
}
