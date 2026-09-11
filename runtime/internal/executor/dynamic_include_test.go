package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internaldebugprotect "github.com/ormasoftchile/yawr/runtime/internal/debugprotect"
	internalexpr "github.com/ormasoftchile/yawr/runtime/internal/expr"
	igov "github.com/ormasoftchile/yawr/runtime/internal/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/governance"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// --- stubs ---

type stubDynamicResolver struct {
	result        *DynamicIncludeResult
	err           error
	calls         []string
	beforeResolve func() error
}

func (s *stubDynamicResolver) Resolve(_ context.Context, renderedRef string) (*DynamicIncludeResult, error) {
	if s.beforeResolve != nil {
		if err := s.beforeResolve(); err != nil {
			return nil, err
		}
	}
	s.calls = append(s.calls, renderedRef)
	return s.result, s.err
}

type capturedCtxRunner struct {
	captured context.Context
	results  []*engine.StepResult
}

type dynamicResolutionCommitter struct {
	state       engine.DynamicIncludeResolutionState
	found       bool
	commitCalls int
	onCommit    func()
}

type resolverIntentCommitter struct {
	prepared bool
}

func (committer *resolverIntentCommitter) PrepareDispatch(
	_ context.Context,
	request engine.DispatchRequest,
) (engine.DispatchState, error) {
	if request.Classification != "read-only" || request.EndpointIdentity != "dynamic-include-resolver" {
		return engine.DispatchState{}, fmt.Errorf("unexpected resolver intent: %#v", request)
	}
	committer.prepared = true
	return engine.DispatchState{
		OccurrenceID:   engine.InteractionPayloadDigest([]byte("resolver intent")),
		IdempotencyKey: engine.InteractionPayloadDigest([]byte("resolver key")),
	}, nil
}

func (committer *dynamicResolutionCommitter) LookupDynamicIncludeResolution(
	_ context.Context,
	_ string,
) (engine.DynamicIncludeResolutionState, bool, error) {
	return committer.state, committer.found, nil
}

func (committer *dynamicResolutionCommitter) CommitDynamicIncludeResolution(
	_ context.Context,
	pin schema.LockedDynamicInclude,
) (engine.DynamicIncludeResolutionState, error) {
	if committer.onCommit != nil {
		committer.onCommit()
	}
	committer.commitCalls++
	committer.state = engine.DynamicIncludeResolutionState{
		ResolutionID: engine.InteractionPayloadDigest([]byte("resolution")), Pin: pin,
	}
	return committer.state, nil
}

type recordingRunbookLoader struct {
	loaded *LoadedRunbook
	paths  []string
}

func (loader *recordingRunbookLoader) Load(_ context.Context, path string) (*LoadedRunbook, error) {
	loader.paths = append(loader.paths, path)
	return loader.loaded, nil
}

func (loader *recordingRunbookLoader) LoadSnapshot(_ context.Context, path string, _ []byte) (*LoadedRunbook, error) {
	loader.paths = append(loader.paths, path)
	return loader.loaded, nil
}

type continueDebugController struct{}

func (continueDebugController) Pause(context.Context, engine.DebugSnapshot) (engine.DebugDecision, error) {
	return engine.DebugDecision{Action: engine.DebugActionContinue}, nil
}

func (r *capturedCtxRunner) run(ctx context.Context, _ SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
	r.captured = ctx
	return r.results, nil
}

type collectEmitter struct {
	events []emittedEvent
}

type emittedEvent struct {
	kind    string
	payload map[string]any
}

func (c *collectEmitter) emit(kind string, payload map[string]any) {
	c.events = append(c.events, emittedEvent{kind, payload})
}

func ctxWithCollector(ctx context.Context, c *collectEmitter) context.Context {
	return WithEventEmitter(ctx, c.emit)
}

// dynamicStep constructs a ResolvedStep with a dynamic include spec.
func dynamicStepWith(stepID, runbookRef string, with map[string]string, onNotFound string) engine.ResolvedStep {
	return engine.ResolvedStep{
		ID:   stepID,
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				RunbookRef:  runbookRef,
				ResolveFrom: schema.ResolveFromCatalog,
				With:        with,
				OnNotFound:  onNotFound,
			},
		},
	}
}

func minimalDynResult() *DynamicIncludeResult {
	return &DynamicIncludeResult{
		QualifiedID:    "pkg/tsg-disk",
		RunbookID:      "tsg-disk",
		RunbookName:    "TSG Disk",
		ContentHash:    strings.Repeat("a", 64),
		AbsPath:        "/resolved/tsg-disk.runbook.yaml",
		PackageName:    "pkg",
		PackageVersion: "1.0.0",
		FileDigest:     "sha256:abc",
		PackageDigest:  "sha256:def",
	}
}

// --- basic success ---

func TestDynamic_Execute_Success(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "pkg/tsg-disk", nil, "")
	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("expected completed, got %v", res.Status)
	}
	if len(resolver.calls) != 1 || resolver.calls[0] != "pkg/tsg-disk" {
		t.Fatalf("expected one resolve call with 'pkg/tsg-disk', got %v", resolver.calls)
	}
}

func TestDynamic_CommitsResolutionBeforeChildExecution(t *testing.T) {
	committed := false
	committer := &dynamicResolutionCommitter{onCommit: func() { committed = true }}
	runner := func(context.Context, SubStepParent, []schema.FlowNode, map[string]any) ([]*engine.StepResult, error) {
		if !committed {
			t.Fatal("dynamic child executed before resolution commit")
		}
		return nil, nil
	}
	resolved := minimalDynResult()
	resolved.FileDigest = engine.InteractionPayloadDigest([]byte("file"))
	resolved.PackageDigest = engine.InteractionPayloadDigest([]byte("package"))
	exec := newIncludeExecutorFull(nil, runner, nil, &stubDynamicResolver{result: resolved}, defaultMaxIncludeDepth, nil)
	ctx := engine.WithDynamicIncludeResolutionCommitter(context.Background(), committer)
	if _, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if committer.commitCalls != 1 {
		t.Fatalf("resolution commit calls = %d, want 1", committer.commitCalls)
	}
}

func TestDynamic_RejectsProtectedHandoffAliasBeforeCustomCommit(t *testing.T) {
	resolved := minimalDynResult()
	resolved.FileDigest = engine.InteractionPayloadDigest([]byte("file"))
	resolved.PackageDigest = engine.InteractionPayloadDigest([]byte("package"))
	resolved.ChildInputs = map[string]*schema.Input{"alias": {Type: "string"}}
	resolved.Flow = []schema.FlowNode{{Step: &schema.Step{
		ID: "continue", Type: schema.StepTypeHandoff,
		HandoffSpec: &schema.HandoffSpec{Handoff: schema.HandoffConfig{
			Runbook: "next.runbook.yaml", Reason: schema.HandoffReason{Code: "continue", Summary: "Continue"},
			With: map[string]string{"server": "${alias}"},
		}},
	}}}
	committer := &dynamicResolutionCommitter{}
	ctx := internaldebugprotect.WithProtection(context.Background(), engine.DebugProtection{
		ProtectedVars: []string{"opaque"}, SecretValues: []string{"never-persist"},
	})
	ctx = engine.WithDynamicIncludeResolutionCommitter(ctx, committer)
	exec := newIncludeExecutorFull(
		&internalexpr.TemplateEvaluator{}, stubRunner(nil), nil,
		&stubDynamicResolver{result: resolved}, defaultMaxIncludeDepth, nil,
	)
	_, err := exec.Execute(
		ctx, dynamicStepWith("s1", "pkg/child", map[string]string{"alias": "${opaque}"}, ""),
		map[string]any{"opaque": "never-persist"},
	)
	if err == nil || strings.Contains(err.Error(), "never-persist") {
		t.Fatalf("Execute error = %v, want value-free protected alias refusal", err)
	}
	if committer.commitCalls != 0 {
		t.Fatalf("custom committer called %d times, want 0", committer.commitCalls)
	}
}

func TestDynamic_CommitsResolverIntentBeforeResolution(t *testing.T) {
	intent := &resolverIntentCommitter{}
	resolved := minimalDynResult()
	resolved.FileDigest = engine.InteractionPayloadDigest([]byte("file"))
	resolved.PackageDigest = engine.InteractionPayloadDigest([]byte("package"))
	resolver := &stubDynamicResolver{result: resolved, beforeResolve: func() error {
		if !intent.prepared {
			return errors.New("resolver ran before durable intent")
		}
		return nil
	}}
	resolution := &dynamicResolutionCommitter{}
	ctx := engine.WithDispatchCommitter(context.Background(), intent)
	ctx = engine.WithDynamicIncludeResolutionCommitter(ctx, resolution)
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)
	if _, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !intent.prepared || resolution.commitCalls != 1 {
		t.Fatalf("intent prepared=%v resolution commits=%d", intent.prepared, resolution.commitCalls)
	}
}

func TestDynamic_ResumeUsesVerifiedPinnedFileWithoutCatalogResolution(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "child.runbook.yaml")
	content := []byte("apiVersion: yawr.runbook/v1\nid: child\nname: Child\nflow: []\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	digest, err := pkgcatalog.FileDigest(path)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	pin := schema.LockedDynamicInclude{
		StepID: "s1", RenderedRef: "pkg/child", QualifiedID: "pkg/child", AbsPath: path,
		PackageName: "pkg", PackageVersion: "1.0.0", FileDigest: digest,
		PackageDigest: engine.InteractionPayloadDigest([]byte("package")),
	}
	committer := &dynamicResolutionCommitter{found: true, state: engine.DynamicIncludeResolutionState{Pin: pin}}
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	loader := &recordingRunbookLoader{loaded: &LoadedRunbook{Flow: []schema.FlowNode{}}}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), loader, resolver, defaultMaxIncludeDepth, nil)
	ctx := engine.WithDynamicIncludeResolutionCommitter(context.Background(), committer)
	if _, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/child", nil, ""), map[string]any{}); err != nil {
		t.Fatalf("Execute pinned: %v", err)
	}
	if len(resolver.calls) != 0 || len(loader.paths) != 1 || loader.paths[0] != path || committer.commitCalls != 0 {
		t.Fatalf("resolver=%#v loader=%#v commits=%d", resolver.calls, loader.paths, committer.commitCalls)
	}
	if err := os.WriteFile(path, append(content, []byte("# changed\n")...), 0o600); err != nil {
		t.Fatalf("change child: %v", err)
	}
	if _, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/child", nil, ""), map[string]any{}); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("Execute changed pin error = %v", err)
	}
}

func TestDynamic_ResumeUsesCommittedClosureAfterPinnedFileDeletion(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "child.runbook.yaml")
	contents := []byte("child source")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	digest, err := pkgcatalog.FileDigest(path)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	resolved := minimalDynResult()
	resolved.AbsPath = path
	resolved.FileDigest = digest
	resolved.Flow = []schema.FlowNode{{Step: &schema.Step{
		ID: "captured-child", Type: schema.StepTypeNoop, NoopSpec: &schema.NoopSpec{},
	}}}
	resolver := &stubDynamicResolver{result: resolved}
	committer := &dynamicResolutionCommitter{}
	var executions []string
	runner := func(_ context.Context, _ SubStepParent, nodes []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
		executions = append(executions, nodes[0].Step.ID)
		return nil, nil
	}
	exec := newIncludeExecutorFull(nil, runner, nil, resolver, defaultMaxIncludeDepth, nil)
	ctx := engine.WithDynamicIncludeResolutionCommitter(context.Background(), committer)
	step := dynamicStepWith("s1", "pkg/tsg-disk", nil, "")
	if _, err := exec.Execute(ctx, step, map[string]any{}); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	committer.found = true
	resolver.calls = nil
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove child: %v", err)
	}
	if _, err := exec.Execute(ctx, step, map[string]any{}); err != nil {
		t.Fatalf("resumed Execute: %v", err)
	}
	if fmt.Sprint(executions) != "[captured-child captured-child]" || len(resolver.calls) != 0 {
		t.Fatalf("executions=%v resolver=%v", executions, resolver.calls)
	}
}

func TestDynamic_PublishesChildDebugProtection(t *testing.T) {
	resolved := minimalDynResult()
	resolved.ChildInputs = map[string]*schema.Input{"api_secret": {Type: "secret"}}
	resolved.ChildGovernance = &schema.GovernanceConfig{Redact: []schema.RedactRule{{
		Pattern: "token-[a-z]+", Replace: "<redacted>",
	}}}
	exec := newIncludeExecutorFull(
		&internalexpr.TemplateEvaluator{}, stubRunner(nil), nil, &stubDynamicResolver{result: resolved}, defaultMaxIncludeDepth, nil,
	)
	var published engine.DebugProtection
	ctx := engine.WithDebugController(context.Background(), continueDebugController{})
	ctx = internaldebugprotect.WithSink(ctx, func(protection engine.DebugProtection) {
		published = protection
	})
	_, err := exec.Execute(
		ctx,
		dynamicStepWith("s1", "pkg/tsg-disk", map[string]string{"api_secret": "Bearer ${root_token}"}, ""),
		map[string]any{"root_token": "secret-value"},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(published.ProtectedVars) != 2 || published.ProtectedVars[0] != "api_secret" || published.ProtectedVars[1] != "root_token" ||
		len(published.SecretValues) != 2 || published.SecretValues[0] != "Bearer secret-value" || published.SecretValues[1] != "secret-value" ||
		len(published.RedactionPatterns) != 1 {
		t.Fatalf("published protection = %#v", published)
	}
}

// --- on_not_found behaviour (B-5/B-6) ---

func TestDynamic_DINC002_Fail(t *testing.T) {
	resolver := &stubDynamicResolver{err: errkit.New("DINC-002", "not found")}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "missing", nil, schema.OnNotFoundFail), map[string]any{})
	if err == nil {
		t.Fatal("expected error for DINC-002 with fail policy")
	}
	assertCode(t, err, "DINC-002")
}

func TestDynamic_DINC002_Continue(t *testing.T) {
	resolver := &stubDynamicResolver{err: errkit.New("DINC-002", "not found")}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	res, err := exec.Execute(context.Background(), dynamicStepWith("s1", "missing", nil, schema.OnNotFoundContinue), map[string]any{})
	if err != nil {
		t.Fatalf("expected no error with on_not_found: continue, got %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("expected skipped, got %v", res.Status)
	}
	if found, ok := res.Vars["runbook_found"].(bool); !ok || found {
		t.Fatalf("expected runbook_found=false, got %v", res.Vars["runbook_found"])
	}
	warning, _ := res.Output["warning"].(string)
	if warning == "" || !strings.Contains(warning, "on_not_found: continue skipped the child runbook") {
		t.Fatalf("expected loud skip warning in output, got %v", res.Output)
	}
	stderr, _ := res.Output["stderr"].(string)
	if stderr != warning {
		t.Fatalf("stderr warning = %q, want %q", stderr, warning)
	}
	if reason, _ := res.Vars["runbook_skipped_reason"].(string); !strings.Contains(reason, "not found") {
		t.Fatalf("expected runbook_skipped_reason to explain miss, got %v", res.Vars["runbook_skipped_reason"])
	}
}

func TestDynamic_DINC003_AlwaysFatal_WithContinue(t *testing.T) {
	resolver := &stubDynamicResolver{err: errkit.New("DINC-003", "ambiguous")}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	// DINC-003 must be fatal even with on_not_found: continue (B-5).
	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "bare", nil, schema.OnNotFoundContinue), map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-003 to be fatal even with on_not_found: continue")
	}
	assertCode(t, err, "DINC-003")
}

func TestDynamic_DINC013_AlwaysFatal_WithContinue(t *testing.T) {
	resolver := &stubDynamicResolver{err: errkit.New("DINC-013", "file missing")}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	// DINC-013 must be fatal even with on_not_found: continue (B-6).
	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg", nil, schema.OnNotFoundContinue), map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-013 to be fatal even with on_not_found: continue")
	}
	assertCode(t, err, "DINC-013")
}

func TestDynamic_OnNotFoundContinue_ErrorBoundary(t *testing.T) {
	tests := []struct {
		name        string
		resolver    DynamicIncludeResolver
		runbookRef  string
		onNotFound  string
		wantSkipped bool
		wantCode    string
		wantErrText string
	}{
		{
			name:        "nil resolver stays hard error even with continue",
			runbookRef:  "pkg/tsg",
			onNotFound:  schema.OnNotFoundContinue,
			wantErrText: "no DynamicIncludeResolver configured",
		},
		{
			name:        "DINC-002 continue is visible skip",
			resolver:    &stubDynamicResolver{err: errkit.New("DINC-002", "not found")},
			runbookRef:  "pkg/missing",
			onNotFound:  schema.OnNotFoundContinue,
			wantSkipped: true,
		},
		{
			name:       "DINC-002 fail policy stays hard error",
			resolver:   &stubDynamicResolver{err: errkit.New("DINC-002", "not found")},
			runbookRef: "pkg/missing",
			wantCode:   "DINC-002",
		},
		{
			name:       "DINC-001 invalid ref stays hard error with continue",
			runbookRef: "bad ref with spaces",
			onNotFound: schema.OnNotFoundContinue,
			wantCode:   "DINC-001",
		},
		{
			name:       "DINC-003 ambiguous bare stays hard error with continue",
			resolver:   &stubDynamicResolver{err: errkit.New("DINC-003", "ambiguous")},
			runbookRef: "bare",
			onNotFound: schema.OnNotFoundContinue,
			wantCode:   "DINC-003",
		},
		{
			name:       "DINC-010 child requires missing stays hard error with continue",
			resolver:   &stubDynamicResolver{err: errkit.New("DINC-010", "child requires missing")},
			runbookRef: "pkg/tsg",
			onNotFound: schema.OnNotFoundContinue,
			wantCode:   "DINC-010",
		},
		{
			name:       "DINC-011 child toolRef missing stays hard error with continue",
			resolver:   &stubDynamicResolver{err: errkit.New("DINC-011", "child toolRef missing")},
			runbookRef: "pkg/tsg",
			onNotFound: schema.OnNotFoundContinue,
			wantCode:   "DINC-011",
		},
		{
			name:       "DINC-013 path missing stays hard error with continue",
			resolver:   &stubDynamicResolver{err: errkit.New("DINC-013", "file missing")},
			runbookRef: "pkg/tsg",
			onNotFound: schema.OnNotFoundContinue,
			wantCode:   "DINC-013",
		},
		{
			name:        "empty rendered ref is DINC-002 skip with continue",
			resolver:    &stubDynamicResolver{result: minimalDynResult()},
			runbookRef:  "${suggested_tsg_id}",
			onNotFound:  schema.OnNotFoundContinue,
			wantSkipped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, tt.resolver, defaultMaxIncludeDepth, nil)
			if tt.runbookRef == "${suggested_tsg_id}" {
				exec = newIncludeExecutorFull(constEval{val: ""}, stubRunner(nil), nil, tt.resolver, defaultMaxIncludeDepth, nil)
			}

			res, err := exec.Execute(context.Background(), dynamicStepWith("s1", tt.runbookRef, nil, tt.onNotFound), map[string]any{})
			if tt.wantSkipped {
				if err != nil {
					t.Fatalf("expected skipped result without error, got %v", err)
				}
				if res.Status != engine.StepStatusSkipped {
					t.Fatalf("expected skipped, got %v", res.Status)
				}
				if found, ok := res.Vars["runbook_found"].(bool); !ok || found {
					t.Fatalf("expected runbook_found=false, got %v", res.Vars["runbook_found"])
				}
				if res.Output["warning"] == "" || res.Output["skip_reason"] != "include_not_found" {
					t.Fatalf("expected visible include_not_found skip output, got %v", res.Output)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected hard error, got result %#v", res)
			}
			if tt.wantErrText != "" {
				if !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErrText, err)
				}
				return
			}
			assertCode(t, err, tt.wantCode)
		})
	}
}

// --- input validation ---

func TestDynamic_InputValidation_RequiredMissing_DINC006(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{"icm_id": {Required: true}}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-006 for missing required input")
	}
	assertCode(t, err, "DINC-006")
}

func TestDynamic_InputValidation_RequiredProvidedViaWith(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{"icm_id": {Required: true}}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "pkg/tsg", map[string]string{"icm_id": "12345"}, "")
	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("expected success when required input provided via with:, got %v", err)
	}
}

func TestDynamic_InputValidation_RequiredProvidedViaParentScope(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{"icm_id": {Required: true}}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "pkg/tsg", nil, "")
	_, err := exec.Execute(context.Background(), step, map[string]any{"icm_id": "99"})
	if err != nil {
		t.Fatalf("expected success when required input found in parent scope, got %v", err)
	}
}

func TestDynamic_InputValidation_EnumViolation_DINC008(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{
		"env": {Enum: schema.EnumConstraint{"prod", "staging"}},
	}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "pkg/tsg", map[string]string{"env": "dev"}, "")
	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-008 for enum violation")
	}
	assertCode(t, err, "DINC-008")
}

func TestDynamic_InputValidation_UnknownWithKey_DINC007_NonFatal(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{}
	resolver := &stubDynamicResolver{result: r}
	coll := &collectEmitter{}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "pkg/tsg", map[string]string{"unknown_key": "val"}, "")
	ctx := ctxWithCollector(context.Background(), coll)
	_, err := exec.Execute(ctx, step, map[string]any{})
	// DINC-W007 is non-fatal per B-15.
	if err != nil {
		t.Fatalf("expected DINC-W007 to be non-fatal, got %v", err)
	}
	var found bool
	for _, ev := range coll.events {
		if ev.kind == "step/output" {
			if code, ok := ev.payload["code"].(string); ok && code == "DINC-W007" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected DINC-W007 warning event, events: %v", coll.events)
	}
}

func TestDynamic_InputValidation_TypeMismatch_DINC009(t *testing.T) {
	r := minimalDynResult()
	r.ChildInputs = map[string]*schema.Input{
		"flag": {Type: "boolean"},
	}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	// Provide a string value for a boolean input.
	step := dynamicStepWith("s1", "pkg/tsg", map[string]string{"flag": "true"}, "")
	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-009 for type mismatch")
	}
	assertCode(t, err, "DINC-009")
}

// --- governance composition (B-8/B-9) ---

func TestDynamic_Governance_DenyCommandsUnion(t *testing.T) {
	r := minimalDynResult()
	r.ChildGovernance = &schema.GovernanceConfig{DenyCommands: []string{"rm"}}
	cr := &capturedCtxRunner{}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	parentGov := &tracepkg.EffectiveGovernancePayload{DenyCommands: []string{"curl"}}
	ctx := withDynGov(context.Background(), parentGov)
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	childGov := dynGovFromCtx(cr.captured)
	if childGov == nil {
		t.Fatal("expected child ctx to carry effective governance")
	}
	if !sliceContains(childGov.DenyCommands, "curl") || !sliceContains(childGov.DenyCommands, "rm") {
		t.Fatalf("expected deny_commands to contain both curl and rm, got %v", childGov.DenyCommands)
	}
}

func TestDynamic_Governance_AllowCommandsIntersection(t *testing.T) {
	r := minimalDynResult()
	r.ChildGovernance = &schema.GovernanceConfig{AllowCommands: []string{"grep", "cat"}}
	cr := &capturedCtxRunner{}
	resolver := &stubDynamicResolver{result: r}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	parentGov := &tracepkg.EffectiveGovernancePayload{AllowCommands: []string{"cat", "ls"}}
	ctx := withDynGov(context.Background(), parentGov)
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	childGov := dynGovFromCtx(cr.captured)
	if childGov == nil {
		t.Fatal("expected child ctx to carry effective governance")
	}
	// intersection of {cat, ls} ∩ {grep, cat} = {cat}
	if len(childGov.AllowCommands) != 1 || childGov.AllowCommands[0] != "cat" {
		t.Fatalf("expected AllowCommands=[cat], got %v", childGov.AllowCommands)
	}
}

// --- cycle and depth ---

func TestDynamic_CycleDetection_DINC004(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	// Pre-seed the chain with the same ID the resolver will return.
	ctx := withIncludeChain(context.Background(), []string{"pkg/tsg-disk"})
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-004 for cycle")
	}
	assertCode(t, err, "DINC-004")
}

func TestDynamic_MaxDepth_AtLimit_Allowed(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, 3, nil)

	// Chain length 2, maxDepth 3 → allowed (2 < 3).
	ctx := withIncludeChain(context.Background(), []string{"a/1", "b/2"})
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("expected success at depth limit, got %v", err)
	}
}

func TestDynamic_MaxDepth_Exceeded_DINC005(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, 3, nil)

	// Chain length 3, maxDepth 3 → rejected (3 >= 3).
	ctx := withIncludeChain(context.Background(), []string{"a/1", "b/2", "c/3"})
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-005 for depth exceeded")
	}
	assertCode(t, err, "DINC-005")
}

// --- trace events ---

func TestDynamic_TraceEvent_IncludeResolved_EmittedOnce(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	coll := &collectEmitter{}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	ctx := ctxWithCollector(context.Background(), coll)
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resolved []emittedEvent
	for _, ev := range coll.events {
		if ev.kind == string(tracepkg.EventKindIncludeResolved) {
			resolved = append(resolved, ev)
		}
	}
	if len(resolved) != 1 {
		t.Fatalf("expected exactly 1 include/resolved event, got %d", len(resolved))
	}
	ev := resolved[0]
	if ev.payload["step_id"] != "s1" {
		t.Errorf("expected step_id=s1, got %v", ev.payload["step_id"])
	}
	if ev.payload["qualified_id"] != "pkg/tsg-disk" {
		t.Errorf("expected qualified_id=pkg/tsg-disk, got %v", ev.payload["qualified_id"])
	}
	if ev.payload["depth"] != 1 {
		t.Errorf("expected depth=1, got %v", ev.payload["depth"])
	}
}

func TestDynamic_TraceEvent_NotFound_Emitted(t *testing.T) {
	resolver := &stubDynamicResolver{err: errkit.New("DINC-002", "not found")}
	coll := &collectEmitter{}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	ctx := ctxWithCollector(context.Background(), coll)
	ctx = engine.WithDispatchExecutionBoundary(ctx, engine.DispatchExecutionBoundary{
		QualifiedNodeID: "each/s1", CallPath: []engine.DebugCallFrame{{StepID: "each"}}, StepID: "s1", Invocation: 1,
	})
	structuralPath := []schema.DynamicIncludeFrameIdentity{{
		QualifiedNodeID: "each", Kind: "iterate", IterationIndex: 2,
	}}
	ctx = engine.WithDynamicIncludeStructuralPath(ctx, structuralPath)
	exec.Execute(ctx, dynamicStepWith("s1", "missing", nil, schema.OnNotFoundContinue), map[string]any{}) //nolint:errcheck

	var notFound []emittedEvent
	for _, ev := range coll.events {
		if ev.kind == string(tracepkg.EventKindIncludeNotFound) {
			notFound = append(notFound, ev)
		}
	}
	if len(notFound) != 1 {
		t.Fatalf("expected 1 include/notFound event, got %d", len(notFound))
	}
	if notFound[0].payload["error_code"] != "DINC-002" {
		t.Errorf("expected error_code=DINC-002, got %v", notFound[0].payload["error_code"])
	}
	if notFound[0].payload["qualified_node_id"] != "each/s1" || notFound[0].payload["invocation"] != 1 ||
		notFound[0].payload["continued"] != true {
		t.Fatalf("not-found occurrence = %#v", notFound[0].payload)
	}
	gotPath, ok := notFound[0].payload["structural_path"].([]schema.DynamicIncludeFrameIdentity)
	if !ok || len(gotPath) != 1 || gotPath[0] != structuralPath[0] {
		t.Fatalf("not-found structural path = %#v", notFound[0].payload["structural_path"])
	}
}

// --- pin recorder ---

func TestDynamic_PinRecorder_CalledOnSuccess(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	var pins []DynamicIncludePin
	recorder := PinRecorder(func(p DynamicIncludePin) { pins = append(pins, p) })
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, recorder)

	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pins) != 1 || pins[0].QualifiedID != "pkg/tsg-disk" || pins[0].StepID != "s1" {
		t.Fatalf("expected pin for s1/pkg/tsg-disk, got %v", pins)
	}
}

// --- child chain propagation ---

func TestDynamic_ChildChainExtended(t *testing.T) {
	cr := &capturedCtxRunner{}
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	ctx := withIncludeChain(context.Background(), []string{"a/parent"})
	_, err := exec.Execute(ctx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chain := includeChainFromCtx(cr.captured)
	if len(chain) != 2 || chain[0] != "a/parent" || chain[1] != "pkg/tsg-disk" {
		t.Fatalf("expected chain [a/parent pkg/tsg-disk], got %v", chain)
	}
}

// --- sibling step depth (regression from Stream 2: dynamic sites must not push/pop exec depth) ---
// This is covered at the planner level; at executor level we verify the static path still works.

// --- helpers ---

func assertCode(t *testing.T, err error, expectedCode string) {
	t.Helper()
	c, ok := err.(errkit.Coder)
	if !ok {
		t.Fatalf("expected error with code %s, got non-Coder error: %v", expectedCode, err)
	}
	if c.Code() != expectedCode {
		t.Fatalf("expected error code %s, got %s: %v", expectedCode, c.Code(), err)
	}
}

func sliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// --- governance enforcement (DEF-003) ---

// mockApprovalGate records whether it was called and optionally returns an error.
type mockApprovalGate struct {
	called bool
	err    error
}

func (g *mockApprovalGate) RequestApproval(_ context.Context, _, _ string) (governance.ApprovalRecord, error) {
	g.called = true
	return governance.ApprovalRecord{}, g.err
}

// constEval is a stub expr.Evaluator that always returns the same string,
// used to simulate a template variable resolving to a known value at runtime.
type constEval struct{ val string }

func (e constEval) Eval(_ string, _ map[string]any) (string, error) { return e.val, nil }

// TestDynamic_Enforcement_DenyCommandsBlockedInCLI verifies that a composed
// governance policy stored in context actually prevents the CLIExecutor from
// running a denied command (deny_commands enforcement).
func TestDynamic_Enforcement_DenyCommandsBlockedInCLI(t *testing.T) {
	pol := igov.BuildPolicy(&schema.GovernanceConfig{DenyCommands: []string{"rm"}})
	ctx := withGovPolicy(context.Background(), pol)

	cliExec := NewCLIExecutor(nil, nil)
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "rm"},
	}
	_, err := cliExec.Execute(ctx, step, nil)
	if err == nil {
		t.Fatal("expected error for denied command, got nil")
	}
	if !strings.Contains(err.Error(), "GOVERNANCE-001") {
		t.Fatalf("expected GOVERNANCE-001 in error, got %v", err)
	}
}

// TestDynamic_Enforcement_PolicyPropagatedToChildCtx verifies that executeDynamic
// builds a governance policy from the composed effective governance and stores it
// in the child context so child executors can enforce it.
func TestDynamic_Enforcement_PolicyPropagatedToChildCtx(t *testing.T) {
	parentGovCtx := withDynGov(context.Background(), &tracepkg.EffectiveGovernancePayload{
		DenyCommands: []string{"kubectl"},
	})

	resolver := &stubDynamicResolver{result: minimalDynResult()}
	cr := &capturedCtxRunner{}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	_, err := exec.Execute(parentGovCtx, dynamicStepWith("s1", "pkg/tsg-disk", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pol := govPolicyFromCtx(cr.captured)
	if pol == nil {
		t.Fatal("expected governance policy in child context, got nil")
	}
	if allowed, _ := pol.CheckCommand("kubectl"); allowed {
		t.Fatal("kubectl should be denied by composed policy propagated from parent")
	}
}

// TestDynamic_Enforcement_RequireApproval_NoGate verifies that a dynamic include
// whose effective governance requires approval fails with DYN-015 when no gate
// is configured.
func TestDynamic_Enforcement_RequireApproval_NoGate(t *testing.T) {
	resolver := &stubDynamicResolver{
		result: &DynamicIncludeResult{
			QualifiedID:     "pkg/tsg",
			AbsPath:         "/path/tsg.runbook.yaml",
			PackageName:     "pkg",
			PackageVersion:  "1.0.0",
			ChildGovernance: &schema.GovernanceConfig{RequireApproval: true},
		},
	}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err == nil {
		t.Fatal("expected error: require_approval with no gate must fail")
	}
	if !strings.Contains(err.Error(), "DYN-015") {
		t.Fatalf("expected DYN-015 in error, got %v", err)
	}
}

// TestDynamic_Enforcement_RequireApproval_GateCalled verifies that when an
// approval gate is configured, it is called before the child runs.
func TestDynamic_Enforcement_RequireApproval_GateCalled(t *testing.T) {
	resolver := &stubDynamicResolver{
		result: &DynamicIncludeResult{
			QualifiedID:     "pkg/tsg",
			AbsPath:         "/path/tsg.runbook.yaml",
			PackageName:     "pkg",
			PackageVersion:  "1.0.0",
			ChildGovernance: &schema.GovernanceConfig{RequireApproval: true},
		},
	}
	gate := &mockApprovalGate{}
	exec := newIncludeExecutorFull(nil, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil).WithApprovalGate(gate)

	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gate.called {
		t.Fatal("expected approval gate to be called for require_approval=true")
	}
}

// TestDynamic_Enforcement_ChildCannotWidenAllowCommands verifies that a child
// declaring broader allow_commands than the parent is intersected down to the
// parent's ceiling (non-widening invariant for allow_commands).
func TestDynamic_Enforcement_ChildCannotWidenAllowCommands(t *testing.T) {
	parentGovCtx := withDynGov(context.Background(), &tracepkg.EffectiveGovernancePayload{
		AllowCommands: []string{"cat"},
	})
	resolver := &stubDynamicResolver{
		result: &DynamicIncludeResult{
			QualifiedID:    "pkg/tsg",
			AbsPath:        "/path/tsg.runbook.yaml",
			PackageName:    "pkg",
			PackageVersion: "1.0.0",
			ChildGovernance: &schema.GovernanceConfig{
				AllowCommands: []string{"cat", "kubectl"},
			},
		},
	}
	cr := &capturedCtxRunner{}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	_, err := exec.Execute(parentGovCtx, dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pol := govPolicyFromCtx(cr.captured)
	if pol == nil {
		t.Fatal("expected governance policy in child context")
	}
	if allowed, _ := pol.CheckCommand("kubectl"); allowed {
		t.Fatal("kubectl must be denied — child cannot widen parent allow_commands from [cat] to [cat,kubectl]")
	}
	if allowed, _ := pol.CheckCommand("cat"); !allowed {
		t.Fatal("cat must remain allowed after intersection")
	}
}

// TestDynamic_Enforcement_AppliedToResolvedChildNotPlaceholder verifies that
// governance is derived from the resolved child's schema, not the placeholder
// IncludeSpec at the include site.
func TestDynamic_Enforcement_AppliedToResolvedChildNotPlaceholder(t *testing.T) {
	resolver := &stubDynamicResolver{
		result: &DynamicIncludeResult{
			QualifiedID:    "pkg/tsg",
			AbsPath:        "/path/tsg.runbook.yaml",
			PackageName:    "pkg",
			PackageVersion: "1.0.0",
			ChildGovernance: &schema.GovernanceConfig{
				DenyCommands: []string{"dangerous-cmd"},
			},
		},
	}
	cr := &capturedCtxRunner{}
	exec := newIncludeExecutorFull(nil, cr.run, nil, resolver, defaultMaxIncludeDepth, nil)

	// The include site (placeholder) carries no governance config; governance
	// must come from the resolved child runbook.
	_, err := exec.Execute(context.Background(), dynamicStepWith("s1", "pkg/tsg", nil, ""), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pol := govPolicyFromCtx(cr.captured)
	if pol == nil {
		t.Fatal("expected governance policy derived from resolved child")
	}
	if allowed, _ := pol.CheckCommand("dangerous-cmd"); allowed {
		t.Fatal("dangerous-cmd must be denied by the resolved child's governance")
	}
}

// --- DEF-005: runtime-rendered empty ref produces DINC-002 ---

// TestDynamic_RenderedEmpty_ProducesDINC002 verifies that a runbook_ref template
// that renders to "" at runtime is caught by ValidateRenderedRef and produces
// DINC-002 — before the resolver is ever called.
func TestDynamic_RenderedEmpty_ProducesDINC002(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(constEval{val: ""}, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	// "${suggested_tsg_id}" is non-empty so IsDynamic()=true; constEval renders it to "".
	step := dynamicStepWith("s1", "${suggested_tsg_id}", nil, "")
	_, err := exec.Execute(context.Background(), step, map[string]any{})
	if err == nil {
		t.Fatal("expected DINC-002 error for runtime-rendered empty ref")
	}
	assertCode(t, err, "DINC-002")
	if len(resolver.calls) != 0 {
		t.Fatalf("resolver must not be called for an empty rendered ref; got %v calls", resolver.calls)
	}
}

// TestDynamic_RenderedEmpty_RecoverableViaContinue verifies that a ref that
// renders to "" at runtime is recoverable under on_not_found: continue, because
// DINC-002 is the only code that on_not_found: continue covers (B-5/B-6).
func TestDynamic_RenderedEmpty_RecoverableViaContinue(t *testing.T) {
	resolver := &stubDynamicResolver{result: minimalDynResult()}
	exec := newIncludeExecutorFull(constEval{val: ""}, stubRunner(nil), nil, resolver, defaultMaxIncludeDepth, nil)

	step := dynamicStepWith("s1", "${suggested_tsg_id}", nil, schema.OnNotFoundContinue)
	res, err := exec.Execute(context.Background(), step, map[string]any{})
	if err != nil {
		t.Fatalf("expected no error with on_not_found: continue for empty rendered ref, got %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("expected skipped, got %v", res.Status)
	}
	if found, ok := res.Vars["runbook_found"].(bool); !ok || found {
		t.Fatalf("expected runbook_found=false, got %v", res.Vars["runbook_found"])
	}
}

// --- run: script governance enforcement ---

// TestDynamic_Enforcement_RunScriptDenied_GOVERNANCE001 verifies that a run:
// script matching a deny_commands pattern is blocked with [GOVERNANCE-001].
// This covers TV-DYN-GOV-005: deny "rm -rf *" must bind scripts that invoke
// "rm -rf ..." even though path.Match's * does not cross /.
func TestDynamic_Enforcement_RunScriptDenied_GOVERNANCE001(t *testing.T) {
	pol := igov.BuildPolicy(&schema.GovernanceConfig{DenyCommands: []string{"rm -rf *"}})
	ctx := withGovPolicy(context.Background(), pol)

	cliExec := NewCLIExecutor(nil, nil)
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "cli",
		Spec: &schema.CLISpec{
			Shell: "sh",
			Run:   "rm -rf /tmp/scratch",
		},
	}
	_, err := cliExec.Execute(ctx, step, nil)
	if err == nil {
		t.Fatal("expected GOVERNANCE-001 error for denied run: script, got nil")
	}
	if !strings.Contains(err.Error(), "GOVERNANCE-001") {
		t.Fatalf("expected GOVERNANCE-001 in error, got %v", err)
	}
}

// TestDynamic_Enforcement_RunScriptDenied_PatternSlashCross verifies that the
// deny pattern "rm -rf *" matches a script with a deeper path like
// "rm -rf /very/deep/nested/path" — i.e. prefix semantics work across slashes.
func TestDynamic_Enforcement_RunScriptDenied_PatternSlashCross(t *testing.T) {
	pol := igov.BuildPolicy(&schema.GovernanceConfig{DenyCommands: []string{"rm -rf *"}})
	ctx := withGovPolicy(context.Background(), pol)

	cliExec := NewCLIExecutor(nil, nil)
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "cli",
		Spec: &schema.CLISpec{
			Shell: "sh",
			Run:   "rm -rf /very/deep/nested/path",
		},
	}
	_, err := cliExec.Execute(ctx, step, nil)
	if err == nil {
		t.Fatal("expected GOVERNANCE-001 for deep-path denied script")
	}
	if !strings.Contains(err.Error(), "GOVERNANCE-001") {
		t.Fatalf("expected GOVERNANCE-001 in error, got %v", err)
	}
}

// TestDynamic_Enforcement_CommandStepStillDenied verifies that the command: form
// governance check was not broken by the run: change — deny "rm" still blocks
// explicit command: "rm" steps.
func TestDynamic_Enforcement_CommandStepStillDenied(t *testing.T) {
	pol := igov.BuildPolicy(&schema.GovernanceConfig{DenyCommands: []string{"rm"}})
	ctx := withGovPolicy(context.Background(), pol)

	cliExec := NewCLIExecutor(nil, nil)
	step := engine.ResolvedStep{
		ID:   "s1",
		Kind: "cli",
		Spec: &schema.CLISpec{Command: "rm"},
	}
	_, err := cliExec.Execute(ctx, step, nil)
	if err == nil {
		t.Fatal("expected GOVERNANCE-001 for command: rm, got nil")
	}
	if !strings.Contains(err.Error(), "GOVERNANCE-001") {
		t.Fatalf("expected GOVERNANCE-001 in error, got %v", err)
	}
}
