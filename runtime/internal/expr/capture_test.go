package expr

import "testing"

func TestTemplateEvaluatorResolveCapturePathNativeGCP(t *testing.T) {
	output := map[string]any{
		"stdout":    `{"status":"ok","items":[{"name":"first"}]}`,
		"stderr":    "warn",
		"exit_code": 7,
	}
	tests := []struct {
		name   string
		source string
		want   any
	}{
		{name: "stdout", source: "stdout", want: output["stdout"]},
		{name: "stderr", source: "stderr", want: "warn"},
		{name: "exit code", source: "exit_code", want: 7},
		{name: "json suffix", source: "json.items[0].name", want: "first"},
	}

	e := &TemplateEvaluator{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := e.ResolveCapturePath(test.source, output)
			if err != nil {
				t.Fatalf("ResolveCapturePath: %v", err)
			}
			if !ok {
				t.Fatal("expected capture to resolve")
			}
			if got != test.want {
				t.Fatalf("expected %#v, got %#v", test.want, got)
			}
		})
	}
	if _, _, err := (&TemplateEvaluator{}).ResolveCapturePath("exitCode", output); err == nil {
		t.Fatal("obsolete exitCode capture alias accepted")
	}
}
