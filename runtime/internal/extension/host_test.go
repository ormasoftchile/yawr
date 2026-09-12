package extension

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/extension"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestHost_Load_Success(t *testing.T) {
	registry := newFakeRegistry()
	host := NewHost(registry)
	manifest := testProjectManifest(t)
	if err := host.Load(context.Background(), manifest); err != nil {
		t.Fatalf("expected load success: %v", err)
	}
	defer func() { _ = host.Shutdown(context.Background()) }()
	if len(host.ContributedTools()) != 1 {
		t.Fatalf("expected 1 contributed tool")
	}
}

func TestHost_Load_NoExtensions(t *testing.T) {
	host := NewHost(newFakeRegistry())
	if err := host.Load(context.Background(), &extension.ProjectManifest{}); err != nil {
		t.Fatalf("expected no error: %v", err)
	}
}

func TestHost_Load_ExtensionFails(t *testing.T) {
	host := NewHost(newFakeRegistry())
	manifest := &extension.ProjectManifest{Extensions: []extension.ExtensionDecl{{Path: "./does-not-exist"}}}
	if err := host.Load(context.Background(), manifest); err == nil {
		t.Fatalf("expected error")
	}
}

func TestHost_ContributedTools_Registered(t *testing.T) {
	registry := newFakeRegistry()
	host := NewHost(registry)
	manifest := testProjectManifest(t)
	if err := host.Load(context.Background(), manifest); err != nil {
		t.Fatalf("expected load success: %v", err)
	}
	defer func() { _ = host.Shutdown(context.Background()) }()
	if _, ok := registry.Lookup("hello"); !ok {
		t.Fatalf("expected registry to include hello")
	}
}

func TestHost_Shutdown_GracefulStop(t *testing.T) {
	registry := newFakeRegistry()
	host := NewHost(registry)
	manifest := testProjectManifest(t)
	if err := host.Load(context.Background(), manifest); err != nil {
		t.Fatalf("expected load success: %v", err)
	}
	if err := host.Shutdown(context.Background()); err != nil {
		t.Fatalf("expected shutdown success: %v", err)
	}
	statuses := host.Status()
	if len(statuses) != 1 || statuses[0].State != extension.StateShutdown {
		t.Fatalf("expected shutdown state")
	}
}

func TestHost_Status_ReflectsState(t *testing.T) {
	host := NewHost(newFakeRegistry())
	manifest := testProjectManifest(t)
	if err := host.Load(context.Background(), manifest); err != nil {
		t.Fatalf("expected load success: %v", err)
	}
	defer func() { _ = host.Shutdown(context.Background()) }()
	statuses := host.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status")
	}
	if statuses[0].Name != "hello-ext" || statuses[0].State != extension.StateLoaded {
		t.Fatalf("unexpected status: %v", statuses[0])
	}
}

func TestHost_ToolRegistry_RegisterCalled(t *testing.T) {
	registry := newFakeRegistry()
	host := NewHost(registry)
	manifest := testProjectManifest(t)
	if err := host.Load(context.Background(), manifest); err != nil {
		t.Fatalf("expected load success: %v", err)
	}
	defer func() { _ = host.Shutdown(context.Background()) }()
	if registry.registerCalls == 0 {
		t.Fatalf("expected Register to be called")
	}
}

func testProjectManifest(t *testing.T) *extension.ProjectManifest {
	t.Helper()
	extDir := os.Getenv("YAWR_EXT_DIR")
	if extDir == "" {
		t.Fatalf("YAWR_EXT_DIR not set")
	}
	return &extension.ProjectManifest{
		Extensions: []extension.ExtensionDecl{{
			Path:   extDir,
			Grants: []string{extension.CapabilityToolRegistration},
		}},
	}
}

type fakeRegistry struct {
	mu            sync.Mutex
	tools         map[string]tool.ToolDef
	registerCalls int
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{tools: make(map[string]tool.ToolDef)}
}

func (r *fakeRegistry) Lookup(name string) (*tool.ToolDef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	def, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	copy := def
	return &copy, true
}

func (r *fakeRegistry) All() []tool.ToolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tool.ToolDef, 0, len(r.tools))
	for _, def := range r.tools {
		out = append(out, def)
	}
	return out
}

func (r *fakeRegistry) Register(def tool.ToolDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[def.Name]; exists {
		return fmt.Errorf("tool %q already registered", def.Name)
	}
	r.registerCalls++
	r.tools[def.Name] = def
	return nil
}
