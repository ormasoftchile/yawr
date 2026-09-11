package main

import (
	"bytes"
	"encoding/json"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// This short-lived tee borrows the configured writer. Only catalog events are
// sent through it; the adapter retains sole ownership of writer shutdown.
type preStartTrace struct {
	writer trace.TraceWriter
	events []engine.Event
}

func (w *preStartTrace) Append(event trace.TraceEvent) error {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	if err := w.writer.Append(event); err != nil {
		return err
	}
	w.events = append(w.events, engine.Event{
		EventID: event.EventID, RunID: event.RunID, RunbookID: event.RunbookID,
		Timestamp: event.Timestamp, Kind: string(event.Kind), Sequence: event.Sequence,
		Payload: payload,
	})
	return nil
}

func (*preStartTrace) Close() error { return nil }
