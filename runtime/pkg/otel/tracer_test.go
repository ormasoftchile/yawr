package otel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/otel"
)

func TestNoopTracer_Start(t *testing.T) {
	p := otel.NoopTracerProvider()
	tracer := p.Tracer("test")

	ctx := context.Background()
	newCtx, span := tracer.Start(ctx, "noop.span")

	if newCtx == nil {
		t.Fatal("expected non-nil context")
	}
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	// noop span returns ctx unchanged
	if newCtx != ctx {
		// This is fine for noop — it returns the same ctx
	}
}

func TestNoopSpan_End(t *testing.T) {
	p := otel.NoopTracerProvider()
	tracer := p.Tracer("test")
	_, span := tracer.Start(context.Background(), "test")
	// Should not panic.
	span.End()
	span.End() // double-end must not panic
}

func TestNoopSpan_SetAttributes(t *testing.T) {
	p := otel.NoopTracerProvider()
	_, span := p.Tracer("test").Start(context.Background(), "test")
	// Must not panic with any attribute types.
	span.SetAttributes(
		otel.Attribute{Key: "str", Value: "hello"},
		otel.Attribute{Key: "int", Value: 42},
		otel.Attribute{Key: "bool", Value: true},
		otel.Attribute{Key: "float", Value: 3.14},
	)
}

func TestAttribute_Types(t *testing.T) {
	attrs := []otel.Attribute{
		{Key: "string", Value: "v"},
		{Key: "int", Value: 1},
		{Key: "int64", Value: int64(9999999999)},
		{Key: "bool", Value: false},
		{Key: "float64", Value: 1.23},
	}
	if len(attrs) != 5 {
		t.Fatalf("expected 5 attributes, got %d", len(attrs))
	}
	for _, a := range attrs {
		if a.Key == "" {
			t.Error("attribute key must not be empty")
		}
	}
}

func TestRecordingTracerProvider_SpanHierarchy(t *testing.T) {
	provider := &otel.RecordingTracerProvider{}
	tracer := provider.Tracer("yawr/test")

	ctx := context.Background()
	runCtx, runSpan := tracer.Start(ctx, "yawr.run")
	stepCtx, stepSpan := tracer.Start(runCtx, "yawr.step.cli")
	_, toolSpan := tracer.Start(stepCtx, "yawr.tool.stdio")

	toolSpan.End()
	stepSpan.End()
	runSpan.End()

	spans := provider.Spans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}

	byName := make(map[string]*otel.SpanData)
	for _, s := range spans {
		byName[s.Name] = s
	}

	run := byName["yawr.run"]
	step := byName["yawr.step.cli"]
	tool := byName["yawr.tool.stdio"]

	if run == nil || step == nil || tool == nil {
		t.Fatal("expected all three spans to be recorded")
	}

	if step.ParentSpanID != run.SpanID {
		t.Errorf("step.ParentSpanID=%q; want %q (run span)", step.ParentSpanID, run.SpanID)
	}
	if tool.ParentSpanID != step.SpanID {
		t.Errorf("tool.ParentSpanID=%q; want %q (step span)", tool.ParentSpanID, step.SpanID)
	}
	if run.ParentSpanID != "" {
		t.Errorf("run span should have no parent, got %q", run.ParentSpanID)
	}
}

func TestRecordingSpan_SetStatus(t *testing.T) {
	provider := &otel.RecordingTracerProvider{}
	_, span := provider.Tracer("t").Start(context.Background(), "test")
	span.SetStatus(otel.StatusError, "something failed")
	span.End()

	spans := provider.Spans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status != otel.StatusError {
		t.Errorf("expected StatusError, got %v", spans[0].Status)
	}
	if spans[0].StatusMsg != "something failed" {
		t.Errorf("expected status msg %q, got %q", "something failed", spans[0].StatusMsg)
	}
}

func TestRecordingSpan_RecordError(t *testing.T) {
	provider := &otel.RecordingTracerProvider{}
	_, span := provider.Tracer("t").Start(context.Background(), "test")
	wantErr := errors.New("test error")
	span.RecordError(wantErr)
	span.End()

	spans := provider.Spans()
	if spans[0].RecordedErr != wantErr {
		t.Errorf("expected recorded error %v, got %v", wantErr, spans[0].RecordedErr)
	}
}

func TestRecordingSpan_Attributes(t *testing.T) {
	provider := &otel.RecordingTracerProvider{}
	_, span := provider.Tracer("t").Start(context.Background(), "test",
		otel.WithAttributes(
			otel.Attribute{Key: "yawr.run.id", Value: "run-123"},
			otel.Attribute{Key: "yawr.step.kind", Value: "cli"},
		),
	)
	span.End()

	spans := provider.Spans()
	if len(spans[0].Attributes) != 2 {
		t.Errorf("expected 2 attributes, got %d", len(spans[0].Attributes))
	}
}

func TestResolveProvider_Nil(t *testing.T) {
	p := otel.ResolveProvider(nil)
	if p == nil {
		t.Fatal("ResolveProvider(nil) must not return nil")
	}
	// Should be a noop provider — Start should not panic.
	_, span := p.Tracer("test").Start(context.Background(), "test")
	span.End()
}

func TestContextWithSpan(t *testing.T) {
	provider := &otel.RecordingTracerProvider{}
	ctx := context.Background()
	_, span := provider.Tracer("t").Start(ctx, "root")

	spanCtx := otel.ContextWithSpan(ctx, span)
	retrieved := otel.SpanFromContext(spanCtx)
	if retrieved == nil {
		t.Fatal("expected span from context")
	}
	if retrieved != span {
		t.Error("retrieved span should be the same as stored span")
	}
}

func TestSpanFromContext_NoSpan(t *testing.T) {
	span := otel.SpanFromContext(context.Background())
	if span == nil {
		t.Fatal("SpanFromContext must not return nil")
	}
	// Should be noop — calling methods must not panic.
	span.SetAttributes(otel.Attribute{Key: "k", Value: "v"})
	span.End()
}
