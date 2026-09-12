package runstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPresentationPreflightBeforeLeaseArtifacts(t *testing.T) {
	cases := []string{
		`{"schema_version":"execution-plan/v77"}`,
		`{"schema_version":"yawr.execution-plan/v1","schema_version":"execution-plan/v2"}`,
		`{"schema_version":"yawr.execution-plan/v1"}{}`,
		`{"schema_version":"yawr.execution-plan/v1","tools":{"db":{"actions":{"run":{"args":{"code":{"type":"string","presentation":{"version":1,"kind":"code","language":"sql"}}}}}}}}`,
	}
	for _, name := range []string{"args", "inputs", "outputs", "default", "presentation", "tools", "actions"} {
		for _, site := range []string{"args", "outputs"} {
			cases = append(cases, fmt.Sprintf(`{"schema_version":"yawr.execution-plan/v1","tools":{%q:{"actions":{%q:{%q:{%q:{"type":"string","presentation":{"version":1,"kind":"code","language":"sql"}}}}}}}}`, name, name, site, name))
		}
	}
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			store := NewDirRunStore(root)
			defer store.Close()
			path := store.PlanPath("invalid")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadDir(root)
			if lease, err := store.AcquireRunLease(context.Background(), "invalid"); err == nil {
				lease.Release()
				t.Fatal("invalid snapshot acquired lease")
			}
			after, _ := os.ReadDir(root)
			names := func(entries []os.DirEntry) []string {
				out := []string{}
				for _, e := range entries {
					out = append(out, e.Name())
				}
				return out
			}
			if !reflect.DeepEqual(names(before), names(after)) {
				t.Fatalf("preflight created lease artifacts: %v -> %v", names(before), names(after))
			}
		})
	}
}
