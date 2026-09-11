package expr

import "testing"

func TestTemplateEvaluatorGISInterpolation(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		vars map[string]any
		want string
	}{
		{name: "plain", tmpl: "plain", vars: map[string]any{"name": "yawr"}, want: "plain"},
		{name: "variable", tmpl: "hello ${name}", vars: map[string]any{"name": "yawr"}, want: "hello yawr"},
		{name: "nested", tmpl: "region=${config.region}", vars: map[string]any{"config": map[string]any{"region": "us-east-1"}}, want: "region=us-east-1"},
		{name: "expression", tmpl: "ok=${str.contains(output, '200')}", vars: map[string]any{"output": "HTTP 200"}, want: "ok=true"},
	}

	e := &TemplateEvaluator{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := e.Eval(test.tmpl, test.vars)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}

func TestTemplateEvaluatorMissingVar(t *testing.T) {
	e := &TemplateEvaluator{}
	_, err := e.Eval("${missing}", map[string]any{})
	if err == nil {
		t.Fatal("expected missing var error")
	}
}
