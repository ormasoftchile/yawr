package eval

import (
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/functions"
	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func TestAuthoringRuntimeInventoryParity(t *testing.T) {
	args := map[string]string{
		"len": "'x'", "now": "",
		"str.startsWith": "'x','x'", "str.endsWith": "'x','x'", "str.contains": "'x','x'",
		"str.toLower": "'x'", "str.toUpper": "'x'", "str.trim": "'x'", "str.length": "'x'",
		"str.trimPrefix": "'x','x'", "str.trimSuffix": "'x','x'",
		"list.contains": "items,1", "list.indexOf": "items,1", "list.length": "items",
		"list.order": "items,'left < right'", "regex.match": "'x','x'",
		"date.compare":     "'2020-01-01T00:00:00Z','2020-01-01T00:00:00Z'",
		"date.diffSeconds": "'2020-01-01 00:00:00','2020-01-01 00:00:00'",
	}
	if len(args) != len(functions.All()) {
		t.Fatal("inventory count drift")
	}
	for _, d := range functions.All() {
		t.Run(d.Name, func(t *testing.T) {
			source, ok := args[d.Name]
			if !ok {
				t.Fatal("uncovered runtime builtin")
			}
			if _, err := parser.Parse(d.Name + "(" + source + ")"); err != nil {
				t.Fatal(err)
			}
			evalString(t, d.Name+"("+source+")", map[string]any{"items": []any{2, 1}})
			ns, name := "", d.Name
			if p := strings.SplitN(d.Name, ".", 2); len(p) == 2 {
				ns, name = p[0], p[1]
			}
			wrong := make([]pjvm.Value, len(d.Parameters())+1)
			for i := range wrong {
				wrong[i] = pjvm.Null()
			}
			if _, err := CallBuiltin(ns, name, wrong, &Scope{}); err == nil {
				t.Fatal("arity drift")
			}
		})
	}
	for _, name := range []string{"len", "str.length"} {
		evalString(t, name+"(items)", map[string]any{"items": []any{1, 2}})
	}
}
