package trace

import tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"

// MultiWriter tees trace events to multiple writers.
type MultiWriter struct {
	writers []tracepkg.TraceWriter
}

// NewMultiWriter constructs a MultiWriter from the provided writers.
func NewMultiWriter(writers ...tracepkg.TraceWriter) *MultiWriter {
	filtered := make([]tracepkg.TraceWriter, 0, len(writers))
	for _, w := range writers {
		if w == nil {
			continue
		}
		filtered = append(filtered, w)
	}
	return &MultiWriter{writers: filtered}
}

// Append writes the event to all writers, returning the first error encountered.
func (m *MultiWriter) Append(event tracepkg.TraceEvent) error {
	var firstErr error
	for _, w := range m.writers {
		if err := w.Append(event); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close closes all writers, returning the first error encountered.
func (m *MultiWriter) Close() error {
	var firstErr error
	for _, w := range m.writers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// LastSequence returns the highest sequence visible in any underlying sink.
func (m *MultiWriter) LastSequence(runID string) int64 {
	var highest int64
	for _, writer := range m.writers {
		if provider, ok := writer.(interface{ LastSequence(string) int64 }); ok {
			if sequence := provider.LastSequence(runID); sequence > highest {
				highest = sequence
			}
		}
	}
	return highest
}
