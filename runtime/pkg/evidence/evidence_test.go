package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestHashFile_ValidFile(t *testing.T) {
	dir := makeWorkDir(t)
	path := filepath.Join(dir, "sample.txt")
	data := []byte("abc")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	digest, size, err := HashFile(path)
	if err != nil {
		t.Fatalf("HashFile: %v", err)
	}
	if size != int64(len(data)) {
		t.Fatalf("expected size %d, got %d", len(data), size)
	}
	const expected = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if digest != expected {
		t.Fatalf("expected digest %s, got %s", expected, digest)
	}
}

func TestHashFile_NotFound(t *testing.T) {
	if _, _, err := HashFile("missing-file.txt"); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func TestHashBytes_Empty(t *testing.T) {
	const expected = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := HashBytes(nil); got != expected {
		t.Fatalf("expected digest %s, got %s", expected, got)
	}
}

func TestEvidenceRecord_JSONRoundTrip(t *testing.T) {
	record := EvidenceRecord{
		Name:       "stdout",
		Kind:       EvidenceKindText,
		Value:      "ok",
		Items:      map[string]string{"step": "checked"},
		Path:       "attachments/abc.txt",
		SHA256:     "deadbeef",
		Size:       42,
		CapturedAt: time.Date(2026, 4, 24, 10, 30, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded EvidenceRecord
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(record, decoded) {
		t.Fatalf("expected %#v, got %#v", record, decoded)
	}
}

func TestEvidenceKind_Values(t *testing.T) {
	kinds := []EvidenceKind{EvidenceKindText, EvidenceKindChecklist, EvidenceKindAttachment, EvidenceKindSnapshot}
	seen := make(map[EvidenceKind]bool, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			t.Fatalf("expected non-empty kind")
		}
		if seen[kind] {
			t.Fatalf("expected distinct kinds, saw duplicate %q", kind)
		}
		seen[kind] = true
	}
}

func makeWorkDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "evidence-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
