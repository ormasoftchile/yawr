package engine

import (
	"context"
	enginepkg "github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/sensitive"
	"strings"
	"unicode/utf16"
)

func attachToolPresentation(ctx context.Context, payload map[string]any) {
	b := enginepkg.ToolPresentationFromContext(ctx)
	if b == nil || b.ToolID == "" {
		return
	}
	e := presentation.ForAction(b.ToolID, b.Action, "frozen", b.SnapshotDigest, b.Definition)
	if b.SnapshotDigest == "" {
		e.Origin = "current"
	}
	if b.Definition == nil {
		e.Reason = "unresolved-dynamic"
	}
	payload["code_presentation"] = e
	status := map[string]string{}
	for _, f := range e.Outputs {
		if f.Presentation != nil {
			status[f.Name] = "unavailable"
			if b.OutputsValidated {
				status[f.Name] = "available"
			}
		}
	}
	payload["output_value_status"] = status
}

// Classification happens after the authoritative protection pass. A descriptor
// alone never makes an output available to a tokenizer.
func classifyPresentationOutput(original, protected map[string]any) map[string]any {
	e, ok := original["code_presentation"].(*presentation.Envelope)
	if !ok {
		return protected
	}
	values, _ := protected["output"].(map[string]any)
	raw, _ := original["output"].(map[string]any)
	status := map[string]string{}
	approved, _ := original["output_value_status"].(map[string]string)
	for _, f := range e.Outputs {
		if f.Presentation == nil {
			continue
		}
		status[f.Name] = "unavailable"
		if sensitive.Name(f.Name) {
			status[f.Name] = "redacted"
			if _, present := values[f.Name]; present {
				copy := make(map[string]any, len(values))
				for name, value := range values {
					copy[name] = value
				}
				copy[f.Name] = "<redacted>"
				values = copy
				protected["output"] = copy
			}
			continue
		}
		if approved[f.Name] != "available" {
			continue
		}
		v, present := values[f.Name]
		if !present {
			status[f.Name] = "absent"
			continue
		}
		s, ok := v.(string)
		if !ok || f.ValueType != "string" {
			continue
		}
		if strings.Contains(s, "<redacted>") || raw[f.Name] != v {
			status[f.Name] = "redacted"
			continue
		}
		if len(utf16.Encode([]rune(s))) > 32768 {
			status[f.Name] = "truncated"
			copy := make(map[string]any, len(values))
			for name, value := range values {
				copy[name] = value
			}
			units := 0
			end := 0
			for i, r := range s {
				size := 1
				if r > 0xffff {
					size = 2
				}
				if units+size > 32768 {
					break
				}
				units += size
				end = i + len(string(r))
			}
			copy[f.Name] = s[:end]
			values = copy
			protected["output"] = copy
			continue
		}
		status[f.Name] = "available"
	}
	protected["output_value_status"] = status
	return protected
}
