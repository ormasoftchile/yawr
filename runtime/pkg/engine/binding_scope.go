package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

type BindingScopeState struct {
	FrameID           string                    `json:"frame_id,omitempty"`
	Initialized       bool                      `json:"initialized"`
	DeclarationDigest string                    `json:"declaration_digest"`
	Invocation        *schema.RunbookInvocation `json:"invocation"`
}

func InvocationDigest(invocation *schema.RunbookInvocation) string {
	data, _ := json.Marshal(invocation)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func CloneBindingScope(scope *BindingScopeState) *BindingScopeState {
	if scope == nil {
		return nil
	}
	copy := *scope
	copy.Invocation = schema.CloneInvocation(scope.Invocation)
	return &copy
}

type bindingScopeKey struct{}

func WithBindingScope(ctx context.Context, scope *BindingScopeState) context.Context {
	return context.WithValue(ctx, bindingScopeKey{}, scope)
}

func BindingScopeFromContext(ctx context.Context) *BindingScopeState {
	scope, _ := ctx.Value(bindingScopeKey{}).(*BindingScopeState)
	return scope
}

// CloneResults returns an independent committed publication.
func (state RunState) CloneResults() (*RunResults, error) { return CloneRunResults(state.Results) }
func (run *Run) CloneResults() (*RunResults, error)       { return CloneRunResults(run.Results) }
