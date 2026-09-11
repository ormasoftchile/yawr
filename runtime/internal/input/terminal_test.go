package input

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

func TestTerminalInputProvider_CoercesCollectorScalarsAndPreservesBufferedResponses(t *testing.T) {
	provider := NewTerminalInputProvider(strings.NewReader("true\n2.5\n7\n1, 2, 3\nnext\n"), &bytes.Buffer{})

	first, err := provider.PromptForm(context.Background(), inputpkg.FormRequest{Fields: []inputpkg.FormField{
		{Name: "enabled", Type: "boolean"},
		{Name: "ratio", Type: "number"},
		{Name: "count", Type: "integer"},
		{Name: "ports", Type: "integer", Multiple: true},
	}})
	if err != nil {
		t.Fatalf("first PromptForm: %v", err)
	}
	wantFirst := map[string]any{
		"enabled": true, "ratio": 2.5, "count": int64(7),
		"ports": []any{int64(1), int64(2), int64(3)},
	}
	if !reflect.DeepEqual(first.Values, wantFirst) {
		t.Fatalf("first values = %#v, want %#v", first.Values, wantFirst)
	}

	second, err := provider.PromptForm(context.Background(), inputpkg.FormRequest{Fields: []inputpkg.FormField{
		{Name: "label", Type: "text"},
	}})
	if err != nil {
		t.Fatalf("second PromptForm: %v", err)
	}
	if got := second.Values["label"]; got != "next" {
		t.Fatalf("second label = %#v, want next", got)
	}
}
