package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func stdioResultsRecord(t *testing.T, value any) *engine.RunResults {
	t.Helper()
	record := &engine.RunResults{SchemaVersion: engine.RunResultsSchemaV1, PublicationID: "publication",
		PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64), CheckpointSequence: 42,
		Origin:  engine.ResultsOrigin{NodeID: "results", Invocation: 1},
		Outputs: map[string]engine.NamedResultValue{"result": {Type: "any", Value: value}}}
	if err := record.Seal(); err != nil {
		t.Fatal(err)
	}
	return record
}

func resultsFrames(t *testing.T, buffer *bytes.Buffer) []map[string]json.RawMessage {
	t.Helper()
	scanner := bufio.NewScanner(buffer)
	scanner.Buffer(make([]byte, 65536), maxStdioFrameBytes)
	var frames []map[string]json.RawMessage
	for scanner.Scan() {
		if len(scanner.Bytes())+1 > maxStdioFrameBytes {
			t.Fatal("oversize frame")
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestStdioResultsCanonicalInlineAndChunks(t *testing.T) {
	for _, value := range []any{
		map[string]any{"z": nil, "a": []any{false, 0, "", []any{}, map[string]any{}, math.Copysign(0, -1), "á😀\u2028<&"}},
		strings.Repeat("é😀", 12000),
		strings.Repeat("á😀", 200000),
	} {
		record := stdioResultsRecord(t, value)
		body, err := engine.CanonicalResultsJSON(record, true)
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		protocol := newStdioProtocol(strings.NewReader(""), &buffer)
		state := engine.RunState{RunID: "native", Status: engine.RunStatusCompleted, Results: record}
		if err := protocol.sendFinished(map[string]any{"type": "run.finished", "runID": state.RunID, "status": state.Status, "steps": []any{}}, state); err != nil {
			t.Fatal(err)
		}
		frames := resultsFrames(t, &buffer)
		terminal := frames[len(frames)-1]
		if string(terminal["type"]) != `"run.finished"` || !protocol.terminalSent {
			t.Fatal("missing terminal")
		}
		if len(frames) == 1 {
			if !bytes.Equal(terminal["results"], body) {
				t.Fatal("inline canonical body changed")
			}
			continue
		}
		var assembled []byte
		for _, frame := range frames[:len(frames)-1] {
			if len(frame) != 8 || string(frame["type"]) != `"run.results.chunk"` {
				t.Fatal("wrong chunk contract")
			}
			var offset, total int
			var data, publicationID, digest string
			json.Unmarshal(frame["offset"], &offset)
			json.Unmarshal(frame["totalBytes"], &total)
			json.Unmarshal(frame["data"], &data)
			json.Unmarshal(frame["publicationID"], &publicationID)
			json.Unmarshal(frame["digest"], &digest)
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil || len(decoded) > resultsdelivery.ChunkBytes || offset != len(assembled) || total != len(body) || publicationID != record.PublicationID || digest != record.Digest {
				t.Fatal("invalid chunk identity/offset/budget")
			}
			assembled = append(assembled, decoded...)
		}
		var reference resultsdelivery.Reference
		if err := json.Unmarshal(terminal["results_ref"], &reference); err != nil {
			t.Fatal(err)
		}
		if reference.TotalBytes != len(body) || reference.Digest != record.Digest || !bytes.Equal(assembled, body) {
			t.Fatal("incomplete canonical document")
		}
		var decoded engine.RunResults
		if json.Unmarshal(assembled, &decoded) != nil || decoded.Validate() != nil {
			t.Fatal("digest excludes digest field")
		}
	}
}

func TestStdioResultsExactInlineBoundary(t *testing.T) {
	frame := map[string]any{"version": stdioProtocolVersion, "type": "run.finished", "runID": "native", "status": "completed", "steps": []any{}}
	base, _ := json.Marshal(frame)
	empty := stdioResultsRecord(t, "")
	body, _ := engine.CanonicalResultsJSON(empty, true)
	padding := maxStdioFrameBytes - len(base) - len(`,"results":`) - len(body) - 1
	for _, delta := range []int{0, 1} {
		record := stdioResultsRecord(t, strings.Repeat("x", padding+delta))
		var buffer bytes.Buffer
		protocol := newStdioProtocol(strings.NewReader(""), &buffer)
		if err := protocol.sendFinished(frame, engine.RunState{RunID: "native", Status: engine.RunStatusCompleted, Results: record}); err != nil {
			t.Fatal(err)
		}
		frames := resultsFrames(t, &buffer)
		if (len(frames) == 1) != (delta == 0) {
			t.Fatalf("boundary %d got %d frames", delta, len(frames))
		}
	}
}

type resultsBrokenWriter struct{ writes int }

func (w *resultsBrokenWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= 2 {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

func TestStdioResultsUnavailableAndTransportFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		record *engine.RunResults
		status engine.RunStatus
		secret string
		reason string
	}{
		{"absent", nil, engine.RunStatusCompleted, "", "no-publication"},
		{"failed", stdioResultsRecord(t, "safe"), engine.RunStatusFailed, "", "execution-not-completed"},
		{"cancelled", stdioResultsRecord(t, "safe"), engine.RunStatusCancelled, "", "execution-not-completed"},
		{"pending", stdioResultsRecord(t, "safe"), engine.RunStatusWaiting, "", "execution-not-completed"},
		{"protected", stdioResultsRecord(t, "synthetic-protected-value"), engine.RunStatusCompleted, "synthetic-protected-value", "protected-content"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buffer bytes.Buffer
			p := newStdioProtocol(strings.NewReader(""), &buffer)
			if test.secret != "" {
				if err := p.configureRedaction([]string{test.secret}, nil); err != nil {
					t.Fatal(err)
				}
			}
			state := engine.RunState{RunID: "native", Status: test.status, Results: test.record, BindingScope: &engine.BindingScopeState{}}
			if err := p.sendFinished(map[string]any{"type": "run.finished", "runID": "native", "status": test.status, "error": map[string]any{"code": "original", "message": "execution stays failed"}}, state); err != nil {
				t.Fatal(err)
			}
			raw := append([]byte(nil), buffer.Bytes()...)
			frames := resultsFrames(t, &buffer)
			if len(frames) != 1 || string(frames[0]["results"]) != "null" || !bytes.Contains(frames[0]["results_unavailable"], []byte(test.reason)) || !bytes.Contains(frames[0]["error"], []byte("original")) {
				t.Fatal("untruthful unavailable/failed terminal")
			}
			if test.secret != "" && bytes.Contains(raw, []byte(test.secret)) {
				t.Fatal("protected content leaked")
			}
		})
	}
	record := stdioResultsRecord(t, strings.Repeat("x", 2<<20))
	writer := &resultsBrokenWriter{}
	p := newStdioProtocol(strings.NewReader(""), writer)
	err := p.sendFinished(map[string]any{"type": "run.finished", "runID": "native", "status": "completed"}, engine.RunState{RunID: "native", Status: engine.RunStatusCompleted, Results: record})
	if err == nil || !strings.Contains(err.Error(), "remains durable") || p.terminalSent {
		t.Fatal("chunk failure swallowed")
	}
	if record.Validate() != nil {
		t.Fatal("transport mutated durable record")
	}
}

func TestStdioResultsMissingAndInvalid(t *testing.T) {
	var buffer bytes.Buffer
	p := newStdioProtocol(strings.NewReader(""), &buffer)
	frame := map[string]any{"type": "run.finished", "runID": "current", "status": "completed"}
	if err := p.sendFinished(frame, engine.RunState{RunID: "current", Status: engine.RunStatusCompleted}); err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(buffer.Bytes(), []byte(`"results":null`)) ||
		!bytes.Contains(buffer.Bytes(), []byte("no-publication")) {
		t.Fatal("missing publication was not explicit")
	}
	buffer.Reset()
	p = newStdioProtocol(strings.NewReader(""), &buffer)
	frame = map[string]any{"type": "run.finished", "runID": "current", "status": "completed"}
	record := stdioResultsRecord(t, false)
	record.Digest = "tampered"
	if err := p.sendFinished(frame, engine.RunState{RunID: "current", Status: engine.RunStatusCompleted, Results: record}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buffer.Bytes(), []byte("invalid-publication")) {
		t.Fatal("invalid record exposed")
	}
}

func TestStdioResultsLargeExecutionErrorRemainsFailed(t *testing.T) {
	message := "original failure: " + strings.Repeat("á😀", maxStdioFrameBytes)
	frame := map[string]any{"type": "run.finished", "runID": "failed", "status": "failed", "error": stdioExecutionError(message)}
	var buffer bytes.Buffer
	p := newStdioProtocol(strings.NewReader(""), &buffer)
	if err := p.sendFinished(frame, engine.RunState{RunID: "failed", Status: engine.RunStatusFailed, BindingScope: &engine.BindingScopeState{}}); err != nil {
		t.Fatal(err)
	}
	frames := resultsFrames(t, &buffer)
	if len(frames) != 1 || string(frames[0]["status"]) != `"failed"` || !bytes.Contains(frames[0]["error"], []byte("original failure")) || !bytes.Contains(frames[0]["error"], []byte(`"messageTruncated":true`)) {
		t.Fatal("large execution diagnostic prevented honest bounded terminal")
	}
}
