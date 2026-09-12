// Package otel provides optional OpenTelemetry span integration for yawr.
//
// yawr works without any OTel SDK. When TracerProvider is nil in EngineConfig,
// a noop tracer is used (zero allocation, no-op spans). To enable tracing,
// wire a real TracerProvider from the OTel SDK (or any compatible implementation)
// into EngineConfig.TracerProvider.
//
// The interfaces in this package are a subset of go.opentelemetry.io/otel/trace,
// making it straightforward to bridge real OTel SDK implementations.
package otel
