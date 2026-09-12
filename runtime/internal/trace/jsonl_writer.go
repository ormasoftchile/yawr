package trace

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// JSONLWriter appends trace events as JSON lines to a file.
type JSONLWriter struct {
	mu           sync.Mutex
	file         *os.File
	enc          *json.Encoder
	syncFile     func() error
	eventIDs     map[string]string
	sequences    map[string]string
	lastSequence map[string]int64
	closed       bool
}

type jsonlRecord struct {
	Seq       int64              `json:"seq"`
	TS        string             `json:"ts"`
	Kind      tracepkg.EventKind `json:"kind"`
	RunID     string             `json:"run_id"`
	RunbookID string             `json:"runbook_id,omitempty"`
	EventID   string             `json:"event_id,omitempty"`
	Payload   json.RawMessage    `json:"payload"`
	Signature string             `json:"sig,omitempty"`
}

// NewJSONLWriter constructs a JSONLWriter for the given file path.
func NewJSONLWriter(path string) (*JSONLWriter, error) {
	if path == "" {
		return nil, errors.New("trace: path is required")
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	writer := &JSONLWriter{
		file: file, syncFile: file.Sync,
		eventIDs: make(map[string]string), sequences: make(map[string]string),
		lastSequence: make(map[string]int64),
	}
	if err := writer.loadIdentityIndex(); err != nil {
		_ = file.Close()
		return nil, err
	}
	writer.enc = json.NewEncoder(file)
	return writer, nil
}

// Append writes a single trace event as a JSON line.
func (w *JSONLWriter) Append(event tracepkg.TraceEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return errors.New("trace: writer is closed")
	}
	ts := event.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	}
	payload := event.Payload
	if payload == nil {
		payload = json.RawMessage("null")
	}
	line := jsonlRecord{
		Seq:       event.Sequence,
		TS:        ts,
		Kind:      event.Kind,
		RunID:     event.RunID,
		RunbookID: event.RunbookID,
		EventID:   event.EventID,
		Payload:   payload,
		Signature: event.Signature,
	}
	digest, err := recordDigest(line)
	if err != nil {
		return err
	}
	duplicate, err := w.checkIdentity(line, digest)
	if err != nil {
		return err
	}
	if duplicate {
		return w.syncFile()
	}
	startOffset, err := w.file.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if err := w.enc.Encode(line); err != nil {
		return w.rollbackAppend(startOffset, err)
	}
	if err := w.syncFile(); err != nil {
		return w.rollbackAppend(startOffset, err)
	}
	w.rememberIdentity(line, digest)
	return nil
}

func (w *JSONLWriter) loadIdentityIndex() error {
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(w.file, 64*1024)
	validOffset := int64(0)
	for {
		line, readErr := reader.ReadBytes('\n')
		if errors.Is(readErr, io.EOF) {
			if len(line) > 0 {
				if err := w.file.Truncate(validOffset); err != nil {
					return err
				}
				if err := w.file.Sync(); err != nil {
					return err
				}
			}
			break
		}
		if readErr != nil {
			return readErr
		}
		validOffset += int64(len(line))
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > 1024*1024 {
			return errors.New("trace: JSONL record exceeds 1 MiB")
		}
		var record jsonlRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		digest, err := recordDigest(record)
		if err != nil {
			continue
		}
		duplicate, err := w.checkIdentity(record, digest)
		if err != nil {
			return err
		}
		if !duplicate {
			w.rememberIdentity(record, digest)
		}
	}
	_, err := w.file.Seek(0, io.SeekEnd)
	return err
}

func (w *JSONLWriter) rollbackAppend(offset int64, appendErr error) error {
	truncateErr := w.file.Truncate(offset)
	_, seekErr := w.file.Seek(0, io.SeekEnd)
	w.enc = json.NewEncoder(w.file)
	syncErr := w.file.Sync()
	return errors.Join(appendErr, truncateErr, seekErr, syncErr)
}

func recordDigest(record jsonlRecord) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(record.Payload))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	canonicalPayload, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	record.Payload = canonicalPayload
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}

func (w *JSONLWriter) checkIdentity(record jsonlRecord, digest string) (bool, error) {
	duplicate := false
	if record.EventID != "" {
		if current, found := w.eventIDs[record.EventID]; found {
			if current != digest {
				return false, fmt.Errorf("trace: event id %q conflicts with existing content", record.EventID)
			}
			duplicate = true
		}
	}
	if key := traceSequenceKey(record); key != "" {
		if current, found := w.sequences[key]; found {
			if current != digest {
				return false, fmt.Errorf("trace: run %q sequence %d conflicts with existing content", record.RunID, record.Seq)
			}
			duplicate = true
		}
	}
	return duplicate, nil
}

func (w *JSONLWriter) rememberIdentity(record jsonlRecord, digest string) {
	if record.EventID != "" {
		w.eventIDs[record.EventID] = digest
	}
	if key := traceSequenceKey(record); key != "" {
		w.sequences[key] = digest
		for {
			next := w.lastSequence[record.RunID] + 1
			if _, found := w.sequences[fmt.Sprintf("%s\x00%d", record.RunID, next)]; !found {
				break
			}
			w.lastSequence[record.RunID] = next
		}
	}
}

// LastSequence returns the highest existing sequence for one run.
func (w *JSONLWriter) LastSequence(runID string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastSequence[runID]
}

func traceSequenceKey(record jsonlRecord) string {
	if record.RunID == "" || record.Seq < 1 {
		return ""
	}
	return fmt.Sprintf("%s\x00%d", record.RunID, record.Seq)
}

// Close flushes and closes the underlying file.
func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	if err := w.syncFile(); err != nil {
		_ = w.file.Close()
		return err
	}
	return w.file.Close()
}
