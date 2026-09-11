package main

import (
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

type preStartTestWriter struct {
	appends, closes int
	err             error
}

func (w *preStartTestWriter) Append(trace.TraceEvent) error {
	w.appends++
	return w.err
}
func (w *preStartTestWriter) Close() error { w.closes++; return nil }

func TestPreStartTraceTeeBorrowsWriterAndPropagatesErrors(t *testing.T) {
	sink := &preStartTestWriter{}
	tee := &preStartTrace{writer: sink}
	event := trace.TraceEvent{Payload: []byte(`{"count":9007199254740993}`)}
	if err := tee.Append(event); err != nil || len(tee.events) != 1 {
		t.Fatalf("capture: %v", err)
	}
	copy(event.Payload, []byte(`{"count":0000000000000000}`))
	if got := tee.events[0].Payload["count"].(interface{ String() string }).String(); got != "9007199254740993" {
		t.Fatalf("payload lost precision/ownership: %s", got)
	}
	if err := tee.Close(); err != nil || sink.closes != 0 {
		t.Fatal("tee closed borrowed writer")
	}
	event.Payload = []byte(`{}`)
	sink.err = errors.New("trace output failed")
	if err := tee.Append(event); !errors.Is(err, sink.err) || len(tee.events) != 1 {
		t.Fatalf("failed event swallowed/retained: %v", err)
	}
}
