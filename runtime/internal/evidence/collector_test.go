package evidence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	evidencepkg "github.com/ormasoftchile/yawr/runtime/pkg/evidence"
)

func TestDefaultCollector_CLI_StdoutStderr(t *testing.T) {
	collector := NewDefaultCollector()
	result := &engine.StepResult{
		Output: map[string]any{
			"stdout": "ok",
			"stderr": "warn",
		},
	}
	records, err := collector.Collect(context.Background(), engine.ResolvedStep{ID: "step", Kind: "cli"}, result, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[0].Kind != evidencepkg.EvidenceKindText || records[1].Kind != evidencepkg.EvidenceKindText {
		t.Fatalf("expected text evidence, got %#v", records)
	}
}

func TestDefaultCollector_ToolResponse(t *testing.T) {
	collector := NewDefaultCollector()
	result := &engine.StepResult{Output: map[string]any{"response": "{\"ok\":true}"}}
	records, err := collector.Collect(context.Background(), engine.ResolvedStep{ID: "step", Kind: "tool"}, result, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(records) != 1 || records[0].Name != "tool_response" {
		t.Fatalf("unexpected records: %#v", records)
	}
}

func TestDefaultCollector_Attachment(t *testing.T) {
	dir := makeCollectorDir(t)
	path := filepath.Join(dir, "artifact.txt")
	if err := os.WriteFile(path, []byte("artifact"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	collector := NewDefaultCollector()
	result := &engine.StepResult{
		Output: map[string]any{"attachments": []any{path}},
	}
	records, err := collector.Collect(context.Background(), engine.ResolvedStep{ID: "step", Kind: "cli"}, result, dir)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(records) != 1 || records[0].Kind != evidencepkg.EvidenceKindAttachment {
		t.Fatalf("unexpected records: %#v", records)
	}
	attachPath := filepath.Join(dir, records[0].Path)
	if _, err := os.Stat(attachPath); err != nil {
		t.Fatalf("expected attachment file: %v", err)
	}
}

func TestDefaultCollector_Checklist(t *testing.T) {
	collector := NewDefaultCollector()
	result := &engine.StepResult{
		Output: map[string]any{
			"checklist": map[string]any{"foo": "checked"},
		},
	}
	records, err := collector.Collect(context.Background(), engine.ResolvedStep{ID: "step", Kind: "manual"}, result, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(records) != 1 || records[0].Kind != evidencepkg.EvidenceKindChecklist {
		t.Fatalf("unexpected records: %#v", records)
	}
	if records[0].Items["foo"] != "checked" {
		t.Fatalf("unexpected checklist items: %#v", records[0].Items)
	}
}

func TestDefaultCollector_NilOutput(t *testing.T) {
	collector := NewDefaultCollector()
	records, err := collector.Collect(context.Background(), engine.ResolvedStep{ID: "step", Kind: "cli"}, &engine.StepResult{}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if records != nil {
		t.Fatalf("expected nil records, got %#v", records)
	}
}

func makeCollectorDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "evidence-collector-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
