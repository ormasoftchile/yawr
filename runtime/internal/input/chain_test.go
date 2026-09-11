package input

import (
	"context"
	"errors"
	"testing"

	inputpkg "github.com/ormasoftchile/yawr/runtime/pkg/input"
)

type stubProvider struct {
	name  string
	resp  *inputpkg.InputResponse
	err   error
	calls int
}

func (s *stubProvider) Provide(ctx context.Context, req inputpkg.InputRequest) (*inputpkg.InputResponse, error) {
	s.calls++
	return s.resp, s.err
}

func (s *stubProvider) Name() string { return s.name }

func TestChainProvider_FirstWins(t *testing.T) {
	first := &stubProvider{name: "first", resp: &inputpkg.InputResponse{Value: "a"}}
	second := &stubProvider{name: "second", resp: &inputpkg.InputResponse{Value: "b"}}
	chain := NewChainProvider(first, second)

	resp, err := chain.Provide(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "a" {
		t.Fatalf("expected first response, got %#v", resp)
	}
	if second.calls != 0 {
		t.Fatalf("expected second provider not called")
	}
}

func TestChainProvider_FallbackSecond(t *testing.T) {
	first := &stubProvider{name: "first"}
	second := &stubProvider{name: "second", resp: &inputpkg.InputResponse{Value: "b"}}
	chain := NewChainProvider(first, second)

	resp, err := chain.Provide(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "b" {
		t.Fatalf("expected second response, got %#v", resp)
	}
}

func TestChainProvider_AllMiss(t *testing.T) {
	first := &stubProvider{name: "first"}
	second := &stubProvider{name: "second"}
	chain := NewChainProvider(first, second)

	_, err := chain.Provide(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider, got %v", err)
	}
}

func TestChainProvider_PropagatesError(t *testing.T) {
	first := &stubProvider{name: "first", err: errors.New("boom")}
	chain := NewChainProvider(first)

	_, err := chain.Provide(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestChainProvider_SkipsErrNoProvider(t *testing.T) {
	first := &stubProvider{name: "first", err: ErrNoProvider}
	second := &stubProvider{name: "second", resp: &inputpkg.InputResponse{Value: "ok"}}
	chain := NewChainProvider(first, second)

	resp, err := chain.Provide(context.Background(), inputpkg.InputRequest{VarName: "x"})
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if resp == nil || resp.Value != "ok" {
		t.Fatalf("expected ok, got %#v", resp)
	}
}
