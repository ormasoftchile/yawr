package pkgcatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDependencyScopeExamplesHaveCompleteLocalClosures(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(cwd)), "examples", "dependency-scopes")
	for _, name := range []string{"static-and-lazy", "dynamic", "parallel", "dynamic-parallel", "dynamic-through-tool"} {
		t.Run(name, func(t *testing.T) {
			options := closureOptions(t, root)
			options.Entrypoint = filepath.Join(root, name+".yawr")
			closure := requireClosure(t, options)
			expected := 5
			if name == "dynamic-through-tool" {
				expected++
			}
			if len(closure.Documents) != expected {
				t.Fatalf("captured %d documents, want %d for the complete invocation closure", len(closure.Documents), expected)
			}
			left := closureDocument(t, closure, filepath.Join(root, "catalog", "left.yawr"))
			right := closureDocument(t, closure, filepath.Join(root, "catalog", "right.yawr"))
			if len(left.Bindings) != 1 || len(right.Bindings) != 1 ||
				left.Bindings[0].Name != "query" || right.Bindings[0].Name != "query" ||
				left.Bindings[0].Def.PackageName != "scope.left" || right.Bindings[0].Def.PackageName != "scope.right" {
				t.Fatal("example siblings do not own distinct query definitions")
			}
			if strings.HasPrefix(name, "dynamic") && len(closure.Targets) != 2 {
				t.Fatalf("dynamic candidates = %d, want 2", len(closure.Targets))
			}
		})
	}
}
