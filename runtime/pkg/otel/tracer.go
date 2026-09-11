package otel

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TracerProvider creates Tracers. When nil, a noopTracerProvider is used.
// This interface is a subset of go.opentelemetry.io/otel/trace.TracerProvider.
type TracerProvider interface {
	Tracer(name string, opts ...TracerOption) Tracer
}

// Tracer creates spans. Maps to OTel's Tracer interface.
type Tracer interface {
	Start(ctx context.Context, spanName string, opts ...SpanStartOption) (context.Context, Span)
}

// Span represents an active operation. Maps to OTel's Span interface.
type Span interface {
	End(opts ...SpanEndOption)
	SetAttributes(kv ...Attribute)
	RecordError(err error, opts ...EventOption)
	SetStatus(code StatusCode, description string)
}

// Attribute is a key-value pair attached to a span.
type Attribute struct {
	Key   string
	Value any
}

// StatusCode indicates span outcome.
type StatusCode int

const (
	StatusUnset StatusCode = iota
	StatusOK
	StatusError
)

// TracerOption configures a Tracer. Reserved for future use.
type TracerOption struct{}

// SpanEndOption configures a span End call. Reserved for future use.
type SpanEndOption struct{}

// EventOption configures a RecordError call. Reserved for future use.
type EventOption struct{}

// spanStartConfig accumulates SpanStartOptions.
type spanStartConfig struct {
	attrs []Attribute
}

// SpanStartOption configures a span Start call.
type SpanStartOption func(*spanStartConfig)

// WithAttributes returns a SpanStartOption that sets initial attributes on a span.
func WithAttributes(attrs ...Attribute) SpanStartOption {
	return func(cfg *spanStartConfig) {
		cfg.attrs = append(cfg.attrs, attrs...)
	}
}

// applyStartOptions applies a list of SpanStartOptions to a config.
func applyStartOptions(opts []SpanStartOption) spanStartConfig {
	var cfg spanStartConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// --- Noop implementations ---

// noopTracerProvider is the default when no TracerProvider is configured.
// All operations are no-ops with zero allocation.
type noopTracerProvider struct{}

func (noopTracerProvider) Tracer(_ string, _ ...TracerOption) Tracer { return noopTracer{} }

// noopTracer returns the context unchanged and a noopSpan.
type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, _ string, _ ...SpanStartOption) (context.Context, Span) {
	return ctx, noopSpan{}
}

// noopSpan is a no-op Span implementation.
type noopSpan struct{}

func (noopSpan) End(...SpanEndOption)              {}
func (noopSpan) SetAttributes(...Attribute)        {}
func (noopSpan) RecordError(error, ...EventOption) {}
func (noopSpan) SetStatus(StatusCode, string)      {}

// NoopTracerProvider returns a TracerProvider whose spans are all no-ops.
// This is used automatically when no TracerProvider is wired in EngineConfig.
func NoopTracerProvider() TracerProvider { return noopTracerProvider{} }

// ResolveProvider returns p if non-nil, otherwise NoopTracerProvider.
func ResolveProvider(p TracerProvider) TracerProvider {
	if p != nil {
		return p
	}
	return NoopTracerProvider()
}

// --- Context span propagation ---

type contextKeyType struct{}

var contextKey contextKeyType

// ContextWithSpan returns a new context carrying span.
func ContextWithSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, contextKey, span)
}

// SpanFromContext returns the Span stored in ctx, or a noopSpan.
func SpanFromContext(ctx context.Context) Span {
	if s, ok := ctx.Value(contextKey).(Span); ok {
		return s
	}
	return noopSpan{}
}

// --- Recording implementation (for tests) ---

// SpanData is a completed span record captured by RecordingTracerProvider.
type SpanData struct {
	Name         string
	SpanID       string
	ParentSpanID string
	Attributes   []Attribute
	Status       StatusCode
	StatusMsg    string
	RecordedErr  error
	StartTime    time.Time
	EndTime      time.Time
}

// RecordingTracerProvider records all completed spans in memory.
// Safe for concurrent use. Intended for use in tests.
type RecordingTracerProvider struct {
	mu    sync.Mutex
	spans []*SpanData
}

// Tracer returns a Tracer that records spans into this provider.
func (r *RecordingTracerProvider) Tracer(_ string, _ ...TracerOption) Tracer {
	return &recordingTracer{provider: r}
}

// Spans returns a snapshot of all completed spans.
func (r *RecordingTracerProvider) Spans() []*SpanData {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*SpanData, len(r.spans))
	copy(out, r.spans)
	return out
}

func (r *RecordingTracerProvider) record(s *SpanData) {
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
}

type recordingTracer struct {
	provider *RecordingTracerProvider
}

func (t *recordingTracer) Start(ctx context.Context, name string, opts ...SpanStartOption) (context.Context, Span) {
	cfg := applyStartOptions(opts)

	parentID := ""
	if parent := SpanFromContext(ctx); parent != nil {
		if rs, ok := parent.(*recordingSpan); ok {
			parentID = rs.data.SpanID
		}
	}

	data := &SpanData{
		Name:         name,
		SpanID:       newSpanID(),
		ParentSpanID: parentID,
		Attributes:   append([]Attribute{}, cfg.attrs...),
		StartTime:    time.Now(),
	}
	span := &recordingSpan{provider: t.provider, data: data}
	return ContextWithSpan(ctx, span), span
}

type recordingSpan struct {
	provider *RecordingTracerProvider
	data     *SpanData
	mu       sync.Mutex
	ended    bool
}

func (s *recordingSpan) End(_ ...SpanEndOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	s.data.EndTime = time.Now()
	s.provider.record(s.data)
}

func (s *recordingSpan) SetAttributes(attrs ...Attribute) {
	s.mu.Lock()
	s.data.Attributes = append(s.data.Attributes, attrs...)
	s.mu.Unlock()
}

func (s *recordingSpan) RecordError(err error, _ ...EventOption) {
	s.mu.Lock()
	s.data.RecordedErr = err
	s.mu.Unlock()
}

func (s *recordingSpan) SetStatus(code StatusCode, msg string) {
	s.mu.Lock()
	s.data.Status = code
	s.data.StatusMsg = msg
	s.mu.Unlock()
}

var spanCounter uint64
var spanMu sync.Mutex

func newSpanID() string {
	spanMu.Lock()
	spanCounter++
	id := spanCounter
	spanMu.Unlock()
	return fmt.Sprintf("span-%06d", id)
}
