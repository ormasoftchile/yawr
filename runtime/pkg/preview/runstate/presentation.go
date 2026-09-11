package runstate

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
)

const PreviewPresentationByteBudget = 64 * 1024

// Project only strings already approved by the execution contract and its
// protection boundary. Presentation never confers approval. Reapplying the
// preview budget is safe, including to replayed JSON-decoded events.
func previewPresentation(original, bounded map[string]any) {
	if original["presentation_diagnostic"] == "invalid-metadata" || original["presentation_diagnostic"] == "limit-exceeded" {
		omitPreviewPresentation(bounded, original["presentation_diagnostic"].(string))
		return
	}
	if _, present := original["code_presentation"]; !present {
		return
	}
	var envelope presentation.Envelope
	encoded, err := json.Marshal(original["code_presentation"])
	if len(encoded) > PreviewPresentationByteBudget {
		omitPreviewPresentation(bounded, "limit-exceeded")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err != nil || decoder.Decode(&envelope) != nil || envelope.Version < 1 {
		omitPreviewPresentation(bounded, "invalid-metadata")
		return
	}
	if len(envelope.Arguments) > presentation.MaxEntries || len(envelope.Outputs) > presentation.MaxEntries {
		omitPreviewPresentation(bounded, "limit-exceeded")
		return
	}
	bounded["code_presentation"] = envelope
	var approved map[string]string
	encoded, _ = json.Marshal(original["output_value_status"])
	if json.Unmarshal(encoded, &approved) != nil {
		omitPreviewPresentation(bounded, "invalid-metadata")
		return
	}
	raw, _ := original["output"].(map[string]any)
	values := map[string]any{}
	status := map[string]string{}
	for _, field := range envelope.Outputs {
		if field.Presentation == nil {
			continue
		}
		name := field.Name
		status[name] = "unavailable"
		switch approved[name] {
		case "absent", "redacted", "unavailable":
			status[name] = approved[name]
		case "available", "truncated":
			value, ok := raw[name].(string)
			if sensitive.Name(name) || (ok && strings.Contains(value, "<redacted>")) {
				status[name] = "redacted"
			} else if ok && field.ValueType == "string" {
				values[name] = value
				status[name] = approved[name]
			}
		}
	}
	output, _ := bounded["output"].(map[string]any)
	if output == nil {
		output = map[string]any{}
	}
	projected := previewCapturesWithBudget(values, PreviewCapturesMaxEntries, PreviewCapturesTotalCharBudget-jsonSize(output))
	restored := 0
	excerpted := false
	for name, state := range status {
		if state != "available" && state != "truncated" {
			continue
		}
		value, ok := projected[name].(string)
		if !ok {
			status[name] = "unavailable"
			continue
		}
		if projected[name+"_preview_truncated"] == true || value != values[name] {
			status[name] = "truncated"
		}
		if _, present := output[name]; !present {
			restored++
		}
		excerpted = excerpted || status[name] == "truncated"
		output[name] = value
	}
	if omitted, ok := integerValue(output["preview_fields_omitted"]); ok && restored > 0 {
		if remaining := int(omitted) - restored; remaining > 0 {
			output["preview_fields_omitted"] = remaining
		} else {
			delete(output, "preview_fields_omitted")
			if !truthy(output["stdout_truncated"]) && !truthy(output["stderr_truncated"]) {
				delete(output, "preview_truncated")
			}
		}
	}
	if excerpted {
		output["preview_truncated"] = true
	}
	bounded["output"] = output
	bounded["output_value_status"] = status
}

func omitPreviewPresentation(bounded map[string]any, reason string) {
	delete(bounded, "code_presentation")
	delete(bounded, "output_value_status")
	delete(bounded, "output")
	bounded["presentation_diagnostic"] = reason
}
