package adapter_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/otel/adapter"
)

// TestNewOTLPTracerProvider_EmptyEndpoint verifies that an empty endpoint returns an error.
func TestNewOTLPTracerProvider_EmptyEndpoint(t *testing.T) {
	_, shutdown, err := adapter.NewOTLPTracerProvider("")
	if err == nil {
		if shutdown != nil {
			shutdown()
		}
		t.Fatal("expected error for empty endpoint, got nil")
	}
}

// TestNewOTLPTracerProvider_Success verifies provider creation with a valid endpoint.
// A local TCP listener is started so the gRPC dial succeeds immediately.
func TestNewOTLPTracerProvider_Success(t *testing.T) {
	// Start a listener that accepts connections (need not speak OTLP).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Accept connections in the background so the gRPC dial doesn't block.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	provider, shutdown, err := adapter.NewOTLPTracerProvider(
		ln.Addr().String(),
		adapter.WithServiceName("test-service"),
		adapter.WithInsecure(),
	)
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown func")
	}
	shutdown()
}

// TestNewOTLPTracerProvider_ShutdownNoOp verifies that calling shutdown twice does not panic.
func TestNewOTLPTracerProvider_ShutdownNoOp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	_, shutdown, err := adapter.NewOTLPTracerProvider(ln.Addr().String())
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider: %v", err)
	}
	shutdown()
	// A second call must not panic (even if the SDK returns an error internally).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second shutdown panicked: %v", r)
		}
	}()
	shutdown()
}

// TestNewOTLPTracerProvider_TracerLifecycle creates a span and verifies no panic.
func TestNewOTLPTracerProvider_TracerLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	provider, shutdown, err := adapter.NewOTLPTracerProvider(ln.Addr().String())
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider: %v", err)
	}
	defer shutdown()

	tracer := provider.Tracer("test-tracer")
	if tracer == nil {
		t.Fatal("expected non-nil tracer")
	}

	ctx, span := tracer.Start(context.Background(), "test-span")
	if ctx == nil {
		t.Fatal("expected non-nil ctx")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}

	span.SetAttributes()
	span.SetStatus(0, "ok")
	span.RecordError(nil)
	span.End()
}

// TestNewOTLPTracerProvider_WithHeaders verifies that headers option is applied without error.
func TestNewOTLPTracerProvider_WithHeaders(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	_, shutdown, err := adapter.NewOTLPTracerProvider(
		ln.Addr().String(),
		adapter.WithHeaders(map[string]string{"x-api-key": "test"}),
	)
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider: %v", err)
	}
	// Give background exporter goroutines a moment before shutdown.
	time.Sleep(10 * time.Millisecond)
	shutdown()
}

// TestNewOTLPTracerProvider_WithTLS_NilConfig verifies that WithTLS(nil) uses
// the system default TLS config and does not panic during provider creation.
func TestNewOTLPTracerProvider_WithTLS_NilConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// WithTLS(nil) must not panic and must select the TLS transport path.
	provider, shutdown, err := adapter.NewOTLPTracerProvider(
		ln.Addr().String(),
		adapter.WithTLS(nil),
	)
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider with nil TLS config: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown func")
	}
	shutdown()
}

// TestNewOTLPTracerProvider_WithTLS_CustomConfig verifies that a custom
// *tls.Config is accepted and wired through without error.
func TestNewOTLPTracerProvider_WithTLS_CustomConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// Use InsecureSkipVerify so the TLS handshake doesn't fail against our
	// plain TCP listener — we only care that the option is wired, not that
	// a full TLS exchange succeeds.
	customCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only

	provider, shutdown, err := adapter.NewOTLPTracerProvider(
		ln.Addr().String(),
		adapter.WithTLS(customCfg),
	)
	if err != nil {
		t.Fatalf("NewOTLPTracerProvider with custom TLS config: %v", err)
	}
	if provider == nil {
		t.Fatal("expected non-nil provider")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown func")
	}
	time.Sleep(10 * time.Millisecond)
	shutdown()
}
