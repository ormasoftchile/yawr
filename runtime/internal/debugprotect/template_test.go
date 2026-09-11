package debugprotect

import (
	"reflect"
	"testing"
)

func TestTemplateVariables(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     []string
		wantAll  bool
	}{
		{name: "transformed", template: "Bearer ${root_token}", want: []string{"root_token"}},
		{name: "calls and paths", template: "${str.trim(vars.root_token)}-${region.name}", want: []string{"region", "root_token"}},
		{name: "complete vars map", template: "${vars}", want: []string{}, wantAll: true},
		{name: "escaped interpolation", template: `\${literal}`, want: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, all, err := TemplateVariables(test.template)
			if err != nil {
				t.Fatalf("TemplateVariables: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) || all != test.wantAll {
				t.Fatalf("TemplateVariables(%q) = %#v, %v; want %#v, %v", test.template, got, all, test.want, test.wantAll)
			}
		})
	}
}
