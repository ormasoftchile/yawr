package trace

import (
	"errors"
	"testing"

	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type stubWriter struct {
	appendCount int
	closeCount  int
	err         error
}

func (s *stubWriter) Append(event tracepkg.TraceEvent) error {
	_ = event
	s.appendCount++
	return s.err
}

func (s *stubWriter) Close() error {
	s.closeCount++
	return s.err
}

func TestMultiWriter_TeesToBoth(t *testing.T) {
	w1 := &stubWriter{}
	w2 := &stubWriter{}
	writer := NewMultiWriter(w1, w2)

	if err := writer.Append(tracepkg.TraceEvent{}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if w1.appendCount != 1 || w2.appendCount != 1 {
		t.Fatalf("expected both writers to receive event")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w1.closeCount != 1 || w2.closeCount != 1 {
		t.Fatalf("expected both writers to close")
	}
}

func TestMultiWriter_OneFailDoesNotStopOther(t *testing.T) {
	failed := &stubWriter{err: errors.New("fail")}
	ok := &stubWriter{}
	writer := NewMultiWriter(failed, ok)

	if err := writer.Append(tracepkg.TraceEvent{}); err == nil {
		t.Fatalf("expected error")
	}
	if ok.appendCount != 1 {
		t.Fatalf("expected other writer to receive event")
	}
}
