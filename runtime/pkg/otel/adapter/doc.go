// Package adapter provides a concrete TracerProvider that exports spans
// via the OpenTelemetry Protocol (OTLP) over gRPC.
//
// Use [NewOTLPTracerProvider] to create a provider from an endpoint address.
// The returned shutdown func must be called on process exit to flush pending
// spans.
//
// This package imports the real OpenTelemetry SDK and is the only place in
// yawr that carries that dependency. Code that does not configure
// --otel-endpoint pays no runtime cost because the SDK is never initialised.
package adapter
