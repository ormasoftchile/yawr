package presentation

import (
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"testing"
)

func TestPresentationProjectionDoesNotAliasFrozenMetadata(t *testing.T) {
	def := &schema.ArgDef{Type: "string", Presentation: &schema.PresentationDescriptor{Version: 1, Kind: "code", Language: "sql"}}
	fields := Fields(map[string]*schema.ArgDef{"code": def})
	fields[0].Presentation.Language = "changed"
	if def.Presentation.Language != "sql" {
		t.Fatal("published field mutated frozen tool")
	}
}
