package input

import (
	"context"
	"errors"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type registryProvider struct {
	name string
	resp *inputpkg.InputResponse
	err  error
}

func (r *registryProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	return r.resp, r.err
}

func (r *registryProvider) Name() string { return r.name }

func TestRegistry_RegisterResolve(t *testing.T) {
	reg := NewRegistry().(*defaultRegistry)
	reg.Register(10, &registryProvider{name: "one", resp: &inputpkg.InputResponse{Value: "value"}})

	resp, err := reg.Resolve(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp == nil || resp.Value != "value" {
		t.Fatalf("expected value, got %#v", resp)
	}
}

func TestRegistry_PriorityOrder(t *testing.T) {
	reg := NewRegistry().(*defaultRegistry)
	reg.Register(20, &registryProvider{name: "low", resp: &inputpkg.InputResponse{Value: "low"}})
	reg.Register(5, &registryProvider{name: "high", resp: &inputpkg.InputResponse{Value: "high"}})

	resp, err := reg.Resolve(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp == nil || resp.Value != "high" {
		t.Fatalf("expected high priority result, got %#v", resp)
	}
}

func TestRegistry_DefaultFallback(t *testing.T) {
	reg := NewRegistry().(*defaultRegistry)
	reg.SetDefault(&registryProvider{name: "default", resp: &inputpkg.InputResponse{Value: "fallback"}})

	resp, err := reg.Resolve(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp == nil || resp.Value != "fallback" {
		t.Fatalf("expected fallback value, got %#v", resp)
	}
}

func TestRegistry_NoProvider(t *testing.T) {
	reg := NewRegistry().(*defaultRegistry)

	_, err := reg.Resolve(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}
}

func TestRegistry_ContinuesOnErrNoProvider(t *testing.T) {
	reg := NewRegistry().(*defaultRegistry)
	reg.Register(1, &registryProvider{name: "skip", err: ErrNoProvider})
	reg.Register(2, &registryProvider{name: "hit", resp: &inputpkg.InputResponse{Value: "ok"}})

	resp, err := reg.Resolve(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resp == nil || resp.Value != "ok" {
		t.Fatalf("expected ok, got %#v", resp)
	}
}
