package tool

import (
	"testing"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestMapRegistry_Lookup_Found(t *testing.T) {
	reg := NewMapRegistry([]toolpkg.ToolDef{{Name: "echo"}})
	def, ok := reg.Lookup("echo")
	if !ok || def == nil || def.Name != "echo" {
		t.Fatalf("expected echo tool definition")
	}
}

func TestMapRegistry_Lookup_NotFound(t *testing.T) {
	reg := NewMapRegistry(nil)
	if _, ok := reg.Lookup("missing"); ok {
		t.Fatalf("expected not found")
	}
}

func TestMapRegistry_All(t *testing.T) {
	reg := NewMapRegistry([]toolpkg.ToolDef{{Name: "a"}, {Name: "b"}})
	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(all))
	}
}

func TestMapRegistry_Register(t *testing.T) {
	reg := NewMapRegistry(nil)
	if err := reg.Register(toolpkg.ToolDef{Name: "new-tool"}); err != nil {
		t.Fatalf("expected register to succeed: %v", err)
	}
	if _, ok := reg.Lookup("new-tool"); !ok {
		t.Fatalf("expected tool to be registered")
	}
}

func TestBuiltinRegistry_HasSlack(t *testing.T) {
	reg := NewBuiltinRegistry()
	if _, ok := reg.Lookup("slack-notify"); !ok {
		t.Fatalf("expected slack-notify in builtin registry")
	}
}

func TestBuiltinRegistry_HasAWS(t *testing.T) {
	reg := NewBuiltinRegistry()
	if _, ok := reg.Lookup("aws"); !ok {
		t.Fatalf("expected aws in builtin registry")
	}
}

func TestBuiltinRegistry_AllStubsPresent(t *testing.T) {
	reg := NewBuiltinRegistry()
	names := []string{"slack-notify", "pagerduty-notify", "alertmanager-notify", "aws", "okta", "palo-alto", "splunk", "email-notify"}
	for _, name := range names {
		if _, ok := reg.Lookup(name); !ok {
			t.Fatalf("missing builtin %s", name)
		}
	}
}
