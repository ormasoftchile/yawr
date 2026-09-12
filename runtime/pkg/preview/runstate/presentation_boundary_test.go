package runstate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func providerPresentation() presentation.Envelope {
	e := presentation.Envelope{
		Version: 1, Status: "resolved", Origin: "frozen", ToolID: "synthetic-reader",
		ToolDigest: "sha256:" + strings.Repeat("b", 64), Action: "read",
		PlanSnapshotDigest: "sha256:" + strings.Repeat("a", 64),
		Arguments:          []presentation.Field{},
		Outputs:            []presentation.Field{},
	}
	for i := 0; i < 42; i++ {
		e.Outputs = append(e.Outputs, presentation.Field{Name: fmt.Sprintf("field%02d", i), ValueType: "string",
			Status: "unavailable", Reason: "missing-descriptor"})
		e.Arguments = append(e.Arguments, presentation.Field{Name: fmt.Sprintf("input%02d", i), ValueType: "string",
			Status: "unavailable", Reason: "missing-descriptor"})
	}
	return e
}

func TestPresentationTypedMetadataIsAtomic(t *testing.T) {
	for _, outcome := range []string{"completed", "failed"} {
		envelope := providerPresentation()
		envelope.Outputs[41].Status, envelope.Outputs[41].Reason = "resolved", ""
		envelope.Outputs[41].Presentation = &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
		for _, approval := range []string{"available", "absent", "redacted", "unavailable", "truncated"} {
			original := map[string]any{
				"status": outcome, "error": "MCP-008: synthetic transport failure",
				"code_presentation": envelope, "output_value_status": map[string]string{"field41": approval},
				"output": map[string]any{"field41": "SELECT 1"}, "evidence": make([]any, 42),
			}
			payload := original
			for pass := 0; pass < 3; pass++ {
				payload = PreviewEventPayload(payload)
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &payload); err != nil {
					t.Fatal(err)
				}
				var got presentation.Envelope
				metadata, _ := json.Marshal(payload["code_presentation"])
				if err := json.Unmarshal(metadata, &got); err != nil || !reflect.DeepEqual(got, envelope) {
					t.Fatalf("pass %d: typed identity/fields changed: %s", pass, metadata)
				}
				if strings.Contains(string(metadata), "_preview_") || payload["code_presentation_preview_truncated"] != nil {
					t.Fatal("generic preview marker contaminated typed metadata")
				}
				if payload["error"] != original["error"] || payload["status"] != outcome {
					t.Fatal("core failure/status changed")
				}
				if payload["output_value_status"].(map[string]any)["field41"] != approval {
					t.Fatal("output approval beyond the old ten-field limit changed")
				}
				output := payload["output"].(map[string]any)
				if approval == "available" || approval == "truncated" {
					if output["field41"] != "SELECT 1" {
						t.Fatal("approved value was not preserved")
					}
				} else if output["field41"] != nil {
					t.Fatal("unapproved code was retained")
				}
			}
		}
	}
}

func TestPresentationOversizeAndOldSentinelsOmitOnlyOptionalProjection(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		metadata     any
	}{
		{"oversize", "limit-exceeded", func() any {
			e := providerPresentation()
			e.Outputs[0].Name = strings.Repeat("x", PreviewPresentationByteBudget)
			return e
		}()},
		{"old-preview", "invalid-metadata", map[string]any{
			"version": 1, "status": "resolved", "outputs_preview_truncated": true,
			"outputs": []any{map[string]any{"_preview_omitted": 32, "_preview_truncated": true}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"code_presentation": tc.metadata, "output": map[string]any{"field41": "DO NOT RETAIN"},
				"output_value_status": map[string]string{"field41": "available"}, "status": "failed", "error": "MCP-008"}
			for pass := 0; pass < 3; pass++ {
				payload = PreviewEventPayload(payload)
				if payload["presentation_diagnostic"] != tc.reason || payload["error"] != "MCP-008" || payload["status"] != "failed" {
					t.Fatalf("wrong bounded fallback: %#v", payload)
				}

				for _, key := range []string{"code_presentation", "output_value_status", "output"} {
					if _, ok := payload[key]; ok {
						t.Fatalf("%s retained with unavailable metadata", key)
					}
				}
			}
		})
	}
}

func TestPresentationManyOutputClassificationsRemainExact(t *testing.T) {
	envelope := providerPresentation()
	envelope.Version = 2 // A closed future envelope remains available for consumer fallback.
	approved, values := map[string]string{}, map[string]any{}
	for i := range envelope.Outputs {
		f := &envelope.Outputs[i]
		f.Status, f.Reason = "resolved", ""
		f.Presentation = &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}
		approved[f.Name] = "redacted"
		values[f.Name] = "DO NOT RETAIN"
	}
	projected := PreviewEventPayload(map[string]any{
		"code_presentation": envelope, "output_value_status": approved, "output": values,
	})
	if !reflect.DeepEqual(projected["output_value_status"], approved) {
		t.Fatal("classification map was truncated")
	}
	encoded, _ := json.Marshal(projected)
	if strings.Contains(string(encoded), "DO NOT RETAIN") {
		t.Fatal("rejected output escaped")
	}
}
