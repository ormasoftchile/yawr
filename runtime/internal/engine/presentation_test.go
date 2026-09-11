package engine

import (
	"context"
	"strings"
	"testing"

	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPresentationOutputSafetyAndOccurrenceCapture(t *testing.T) {
	def := &schema.ToolDef{Name: "db", Actions: map[string]*schema.ToolAction{"run": {Outputs: map[string]*schema.ArgDef{"code": {Type: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}}}}}}
	parent := enginepkg.WithToolPresentationCapture(enginepkg.WithPlanTools(context.Background(), map[string]*schema.ToolDef{"db": def}), "sha256:snapshot")
	enginepkg.RecordToolPresentation(parent, "db", "run", def)
	child := enginepkg.WithToolPresentationCapture(parent, "sha256:snapshot")
	enginepkg.RecordToolPresentation(child, "child", "run", nil)
	if enginepkg.ToolPresentationFromContext(parent).ToolID != "db" {
		t.Fatal("nested capture overwrote wrapper")
	}
	payload := map[string]any{"output": map[string]any{"code": "SELECT credential-marker", "stdout": "SELECT stdout"}}
	attachToolPresentation(parent, payload)
	unapproved := classifyPresentationOutput(payload, redactProtectedEventPayload(payload, enginepkg.DebugProtection{}))
	if unapproved["output_value_status"].(map[string]string)["code"] != "unavailable" {
		t.Fatal("unclassified result made available")
	}
	enginepkg.ApproveToolPresentationOutputs(parent)
	attachToolPresentation(parent, payload)
	protected := classifyPresentationOutput(payload, redactProtectedEventPayload(payload, enginepkg.DebugProtection{SecretValues: []string{"credential-marker"}}))
	status := protected["output_value_status"].(map[string]string)
	if status["code"] != "redacted" || status["stdout"] != "" {
		t.Fatalf("unsafe status: %#v", status)
	}
	for _, value := range []string{"", strings.Repeat("a", 40000)} {
		payload = map[string]any{"output": map[string]any{"code": value}}
		attachToolPresentation(parent, payload)
		protected = classifyPresentationOutput(payload, redactProtectedEventPayload(payload, enginepkg.DebugProtection{}))
		status = protected["output_value_status"].(map[string]string)
		if value == "" && status["code"] != "available" {
			t.Fatal("empty string unavailable")
		}
		if value != "" && (status["code"] != "truncated" || len(protected["output"].(map[string]any)["code"].(string)) != 32768) {
			t.Fatal("safe excerpt not bounded")
		}
	}
}
