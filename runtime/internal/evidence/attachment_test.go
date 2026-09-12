package evidence

import (
	"os"
	"path/filepath"
	"testing"

	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
)

func TestAttachmentStore_CopyAndHash(t *testing.T) {
	dir := makeAttachmentDir(t)
	source := filepath.Join(dir, "artifact.txt")
	if err := os.WriteFile(source, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewAttachmentStore(dir)
	record, err := store.Store(source)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if record.Kind != evidencepkg.EvidenceKindAttachment {
		t.Fatalf("expected attachment kind, got %v", record.Kind)
	}
	if record.SHA256 == "" || record.Size == 0 {
		t.Fatalf("expected sha and size, got %#v", record)
	}
	dest := filepath.Join(dir, record.Path)
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected attachment file: %v", err)
	}
}

func TestAttachmentStore_Dedup(t *testing.T) {
	dir := makeAttachmentDir(t)
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	content := []byte("same-content")
	if err := os.WriteFile(fileA, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(fileB, content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := NewAttachmentStore(dir)
	recA, err := store.Store(fileA)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	recB, err := store.Store(fileB)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if recA.Path != recB.Path {
		t.Fatalf("expected deduped paths, got %s vs %s", recA.Path, recB.Path)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "attachments"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one attachment file, got %d", len(entries))
	}
}

func TestAttachmentStore_MissingFile(t *testing.T) {
	store := NewAttachmentStore(makeAttachmentDir(t))
	if _, err := store.Store("missing.txt"); err == nil {
		t.Fatalf("expected error for missing file")
	}
}

func makeAttachmentDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "evidence-attach-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
