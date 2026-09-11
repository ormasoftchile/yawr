package expr

import "testing"

func TestSimpleConditionEvaluator(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		vars      map[string]any
		want      bool
		wantErr   bool
	}{
		{
			name:      "equality",
			condition: `env == "prod"`,
			vars:      map[string]any{"env": "prod"},
			want:      true,
		},
		{
			name:      "inequality",
			condition: `env != "dev"`,
			vars:      map[string]any{"env": "prod"},
			want:      true,
		},
		{
			name:      "numeric greater than",
			condition: "count > 1",
			vars:      map[string]any{"count": 5},
			want:      true,
		},
		{
			name:      "numeric less than",
			condition: "count < 10",
			vars:      map[string]any{"count": 5},
			want:      true,
		},
		{
			name:      "numeric greater or equal",
			condition: "count >= 5",
			vars:      map[string]any{"count": 5},
			want:      true,
		},
		{
			name:      "numeric less or equal",
			condition: "count <= 5",
			vars:      map[string]any{"count": 5},
			want:      true,
		},
		{
			name:      "boolean and",
			condition: `env == "prod" and count > 1`,
			vars:      map[string]any{"env": "prod", "count": 5},
			want:      true,
		},
		{
			name:      "boolean or",
			condition: `env == "prod" or env == "staging"`,
			vars:      map[string]any{"env": "prod"},
			want:      true,
		},
		{
			name:      "boolean not",
			condition: "not flag",
			vars:      map[string]any{"flag": false},
			want:      true,
		},
		{
			name:      "string contains",
			condition: `str.contains(s, "sub")`,
			vars:      map[string]any{"s": "substring"},
			want:      true,
		},
		{
			name:      "empty condition",
			condition: "",
			vars:      map[string]any{},
			want:      true,
		},
		{
			name:      "nested grouping",
			condition: `(env == "prod" or env == "staging") and count > 0`,
			vars:      map[string]any{"env": "prod", "count": 1},
			want:      true,
		},
		{
			name:      "non-boolean error",
			condition: `env + " world"`,
			vars:      map[string]any{"env": "hello"},
			wantErr:   true,
		},
		{
			name:      "vars.field syntax (backward compat)",
			condition: `vars.all_passed == "dns_fail"`,
			vars:      map[string]any{"all_passed": "dns_fail"},
			want:      true,
		},
		{
			name:      "vars.field numeric",
			condition: `vars.count > 40`,
			vars:      map[string]any{"count": 42},
			want:      true,
		},
		{
			name:      "mixed direct and vars. syntax",
			condition: `vars.status == "ok" and count > 10`,
			vars:      map[string]any{"status": "ok", "count": 15},
			want:      true,
		},
		// Regression tests for the vars.all_passed bug
		{
			name:      "bare variable match true - dns_fail",
			condition: `all_passed == "dns_fail"`,
			vars:      map[string]any{"all_passed": "dns_fail"},
			want:      true,
		},
		{
			name:      "bare variable match false - all_pass",
			condition: `all_passed == "dns_fail"`,
			vars:      map[string]any{"all_passed": "all_pass"},
			want:      false,
		},
		{
			name:      "bare variable wrong value",
			condition: `all_passed == "routing_fail"`,
			vars:      map[string]any{"all_passed": "dns_fail"},
			want:      false,
		},
		{
			name:      "wrapper syntax rejected",
			condition: `{{ all_passed == "dns_fail" }}`,
			vars:      map[string]any{"all_passed": "dns_fail"},
			wantErr:   true,
		},
		{
			name:      "vars prefix with dns_fail (defensive fix)",
			condition: `vars.all_passed == "dns_fail"`,
			vars:      map[string]any{"all_passed": "dns_fail"},
			want:      true,
		},
		{
			name:      "vars prefix with routing_fail (defensive fix)",
			condition: `vars.all_passed == "routing_fail"`,
			vars:      map[string]any{"all_passed": "routing_fail"},
			want:      true,
		},
		{
			name:      "vars prefix mismatch (defensive fix)",
			condition: `vars.all_passed == "dns_fail"`,
			vars:      map[string]any{"all_passed": "all_pass"},
			want:      false,
		},
		{
			name:      "boolean variable true",
			condition: "approved",
			vars:      map[string]any{"approved": true},
			want:      true,
		},
		{
			name:      "boolean variable false",
			condition: "approved",
			vars:      map[string]any{"approved": false},
			want:      false,
		},
		{
			name:      "empty condition with nil vars",
			condition: "",
			vars:      nil,
			want:      true,
		},
		// str.* namespace tests
		{
			name:      "str.contains match",
			condition: `str.contains(s, "sub")`,
			vars:      map[string]any{"s": "substring"},
			want:      true,
		},
		{
			name:      "str.contains no match",
			condition: `str.contains(s, "xyz")`,
			vars:      map[string]any{"s": "substring"},
			want:      false,
		},
		{
			name:      "str.startsWith match",
			condition: `str.startsWith(s, "sub")`,
			vars:      map[string]any{"s": "substring"},
			want:      true,
		},
		{
			name:      "str.startsWith no match",
			condition: `str.startsWith(s, "xyz")`,
			vars:      map[string]any{"s": "substring"},
			want:      false,
		},
		{
			name:      "str.endsWith match",
			condition: `str.endsWith(s, "ing")`,
			vars:      map[string]any{"s": "substring"},
			want:      true,
		},
		{
			name:      "str.endsWith no match",
			condition: `str.endsWith(s, "xyz")`,
			vars:      map[string]any{"s": "substring"},
			want:      false,
		},
		{
			name:      "str.toLower",
			condition: `str.toLower(s) == "hello"`,
			vars:      map[string]any{"s": "HELLO"},
			want:      true,
		},
		{
			name:      "str.toUpper",
			condition: `str.toUpper(s) == "HELLO"`,
			vars:      map[string]any{"s": "hello"},
			want:      true,
		},
		{
			name:      "str.trim",
			condition: `str.trim(s) == "test"`,
			vars:      map[string]any{"s": "  test  "},
			want:      true,
		},
		{
			name:      "str.contains with negation",
			condition: `not str.contains(output, "200")`,
			vars:      map[string]any{"output": "404 Not Found"},
			want:      true,
		},
		{
			name:      "str.contains in complex condition",
			condition: `str.contains(dns_output, "Address") and status == "ok"`,
			vars:      map[string]any{"dns_output": "Address: 1.2.3.4", "status": "ok"},
			want:      true,
		},
	}

	e := NewSimpleConditionEvaluator(nil)
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			got, err := e.EvalBool(test.condition, test.vars)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", test.condition)
				}
				return
			}
			if err != nil {
				t.Fatalf("EvalBool: %v", err)
			}
			if got != test.want {
				t.Fatalf("expected %t, got %t", test.want, got)
			}
		})
	}
}

func TestSimpleConditionEvaluatorNativeGXL(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		vars      map[string]any
		want      bool
	}{
		{name: "equality", condition: `env == "prod"`, vars: map[string]any{"env": "prod"}, want: true},
		{name: "and", condition: `env == "prod" and count > 1`, vars: map[string]any{"env": "prod", "count": 5}, want: true},
		{name: "not", condition: `not ready`, vars: map[string]any{"ready": false}, want: true},
		{name: "vars namespace", condition: `vars.status == "ok"`, vars: map[string]any{"status": "ok"}, want: true},
		{name: "str namespace", condition: `str.contains(output, "200")`, vars: map[string]any{"output": "HTTP 200"}, want: true},
	}

	e := NewSimpleConditionEvaluator(nil)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := e.EvalBool(test.condition, test.vars)
			if err != nil {
				t.Fatalf("EvalBool: %v", err)
			}
			if got != test.want {
				t.Fatalf("expected %t, got %t", test.want, got)
			}
		})
	}
}
