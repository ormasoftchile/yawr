package hostaction

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestGenericV1JSONShape(t *testing.T) {
	requestJSON, err := json.Marshal(Request{
		Capability: "product.open-resource",
		Payload:    map[string]any{"resource": "incident-42"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	responseJSON, err := json.Marshal(Response{
		Status: StatusCompleted,
		Result: map[string]any{"state": "opened"},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var request, response map[string]any
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request, map[string]any{
		"capability": "product.open-resource",
		"request":    map[string]any{"resource": "incident-42"},
	}) {
		t.Fatalf("request wire shape = %#v", request)
	}
	if !reflect.DeepEqual(response, map[string]any{
		"status": "completed", "result": map[string]any{"state": "opened"},
	}) {
		t.Fatalf("response wire shape = %#v", response)
	}
}

func TestValidatePayloadAcceptsBoundedJSON(t *testing.T) {
	payload := map[string]any{
		"resource": "incident-42",
		"options": map[string]any{
			"focus":  true,
			"labels": []any{"primary", "westus"},
		},
	}
	if err := ValidatePayload(payload); err != nil {
		t.Fatalf("ValidatePayload: %v", err)
	}
}

func TestValidatePayloadRejectsExceededLimits(t *testing.T) {
	deep := map[string]any{"leaf": "value"}
	for i := 0; i < MaxPayloadDepth; i++ {
		deep = map[string]any{"nested": deep}
	}

	tests := map[string]map[string]any{
		"depth":  deep,
		"string": {"value": strings.Repeat("x", MaxPayloadStringBytes+1)},
		"array":  {"value": make([]any, MaxPayloadArrayItems+1)},
		"key":    {strings.Repeat("k", MaxPayloadKeyBytes+1): true},
		"bytes":  {"values": repeatedStrings(MaxPayloadArrayItems, MaxPayloadStringBytes)},
	}
	tooManyProperties := make(map[string]any, MaxPayloadObjectProperties+1)
	for i := 0; i <= MaxPayloadObjectProperties; i++ {
		tooManyProperties[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = i
	}
	tests["properties"] = tooManyProperties

	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePayload(payload); err == nil {
				t.Fatal("expected bounded payload rejection")
			}
		})
	}
}

func repeatedStrings(count, size int) []any {
	values := make([]any, count)
	for i := range values {
		values[i] = strings.Repeat("x", size)
	}
	return values
}
