package runstate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestPresentationFinalPreview(t *testing.T) {
	for _, language := range []string{"sql", "kql", "powershell"} {
		for _, tc := range []struct{ status, value, want string }{
			{"available", "SELECT 🚀", "available"},
			{"available", "", "available"},
			{"available", "<redacted>", "redacted"},
			{"unavailable", "denied", "unavailable"},
			{"redacted", "secret", "redacted"},
			{"absent", "", "absent"},
			{"available", strings.Repeat("x", 9000), "truncated"},
		} {
			e := &presentation.Envelope{Version: 1, Outputs: []presentation.Field{{Name: "code", ValueType: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: language}}}}
			payload := map[string]any{"code_presentation": e, "output": map[string]any{"code": tc.value}, "output_value_status": map[string]string{"code": tc.status}}
			for pass := 0; pass < 3; pass++ {
				payload = PreviewEventPayload(payload)
				encoded, _ := json.Marshal(payload)
				json.Unmarshal(encoded, &payload)
				status := payload["output_value_status"].(map[string]any)["code"]
				output := payload["output"].(map[string]any)
				if status != tc.want {
					t.Fatalf("%s pass %d: status %v want %s", language, pass, status, tc.want)
				}
				if tc.want == "available" {
					if output["code"] != tc.value {
						t.Fatal("available without exact safe string")
					}
					if output["preview_fields_omitted"] != nil || output["preview_truncated"] != nil {
						t.Fatal("restored complete output retains false omission markers")
					}
				} else if tc.want != "truncated" && output["code"] != nil {
					t.Fatal("rejected value published")
				}
			}
		}
	}
}
