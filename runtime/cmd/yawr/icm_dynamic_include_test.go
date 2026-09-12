package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adapter "github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalreplay "github.com/ormasoftchile/yawr/runtime/internal/replay"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	tracepkg "github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// noopSubStepRunnerICM is a SubStepRunner that returns empty results.
func noopSubStepRunnerICM(_ context.Context, _ internalexecutor.SubStepParent, _ []schema.FlowNode, _ map[string]any) ([]*engine.StepResult, error) {
	return nil, nil
}

func buildICMTestCatalog(t *testing.T) (*pkgcatalog.Catalog, string) {
	t.Helper()
	ws := t.TempDir()
	pkgDir := ws
	rbDir := filepath.Join(pkgDir, "runbooks")
	if err := os.MkdirAll(rbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := "apiVersion: yawr.tool-package/v1\nmeta:\n  name: contoso-tsgs\n  version: \"1.0.0\"\nexports:\n  runbooks:\n    - id: tsg-network-resolve\n      path: runbooks/tsg-network-resolve.runbook.yaml\n"
	childRunbook := "apiVersion: yawr.runbook/v1\nid: tsg-network-resolve\nname: TSG Network Resolve\nflow: []\n"

	manifestPath := filepath.Join(pkgDir, "yawr-package.yaml")
	childPath := filepath.Join(rbDir, "tsg-network-resolve.runbook.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(childRunbook), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot: ws,
		ProjectRequires: []*schema.PackageRequirement{
			{Package: "contoso-tsgs", Version: "1.0.0", Path: "."},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build: %v", errs)
	}
	return cat, childPath
}

type icmCapturedEvent struct {
	kind    string
	payload map[string]any
}

type trackingResolver struct {
	callCount int
	inner     internalexecutor.DynamicIncludeResolver
}

func (r *trackingResolver) Resolve(ctx context.Context, renderedRef string) (*internalexecutor.DynamicIncludeResult, error) {
	r.callCount++
	return r.inner.Resolve(ctx, renderedRef)
}

type icmRunbookExport struct {
	ID      string
	RelPath string
	Content string
}

type icmPackageSpec struct {
	RelDir  string
	Name    string
	Version string
	Exports []icmRunbookExport
}

type capturingFileLoader struct {
	loadedPath    string
	loadedContent string
}

func (l *capturingFileLoader) Load(_ context.Context, absPath string) (*internalexecutor.LoadedRunbook, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	l.loadedPath = absPath
	l.loadedContent = string(data)
	return &internalexecutor.LoadedRunbook{}, nil
}

func newICMIncludeStep(runbookRef, onNotFound string) engine.ResolvedStep {
	return engine.ResolvedStep{
		ID:   "call-tsg",
		Kind: "include",
		Spec: &schema.IncludeSpec{
			Include: schema.IncludeConfig{
				RunbookRef:  runbookRef,
				ResolveFrom: schema.ResolveFromCatalog,
				OnNotFound:  onNotFound,
			},
		},
	}
}

func assertICMErrCode(t *testing.T, err error, want string) {
	t.Helper()
	var ekErr *errkit.Error
	if !errors.As(err, &ekErr) {
		t.Fatalf("expected errkit error %s, got %T: %v", want, err, err)
	}
	if ekErr.Code() != want {
		t.Fatalf("expected error code %s, got %s: %v", want, ekErr.Code(), err)
	}
}

func normalizeICMPath(path string) string {
	if eval, err := filepath.EvalSymlinks(path); err == nil {
		path = eval
	}
	return filepath.Clean(path)
}

func assertICMSamePath(t *testing.T, got, want string) {
	t.Helper()
	if !strings.EqualFold(normalizeICMPath(got), normalizeICMPath(want)) {
		t.Fatalf("path mismatch: got %q, want %q", got, want)
	}
}

func newDryRunExecutorsICM(t *testing.T, resolver internalexecutor.DynamicIncludeResolver, pinRecorder internalexecutor.PinRecorder) (engine.ExecutorRegistry, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		Mode:                   "dry-run",
		TraceFile:              filepath.Join(dir, "trace.jsonl"),
		RunDir:                 filepath.Join(dir, "runs"),
		ToolScanDir:            dir,
		DynamicIncludeResolver: resolver,
		PinRecorder:            pinRecorder,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig(dry-run): %v", err)
	}
	return cfg.Executors, shutdown
}

func newRecursiveIncludeRegistryICM(
	resolver internalexecutor.DynamicIncludeResolver,
	pinRecorder internalexecutor.PinRecorder,
	maxDepth int,
) *internalexecutor.MapRegistry {
	var reg *internalexecutor.MapRegistry
	runner := func(ctx context.Context, parent internalexecutor.SubStepParent, steps []schema.FlowNode, vars map[string]any) ([]*engine.StepResult, error) {
		var results []*engine.StepResult
		for _, node := range steps {
			if node.Step == nil || node.Step.Type != schema.StepTypeInclude || node.Step.IncludeSpec == nil {
				continue
			}
			res, err := reg.Lookup("include").Execute(ctx, engine.ResolvedStep{
				ID:         node.Step.ID,
				Kind:       "include",
				Spec:       node.Step.IncludeSpec,
				NestDepth:  parent.NestDepth,
				ParentID:   parent.ID,
				ParentKind: parent.Kind,
			}, vars)
			if err != nil {
				return nil, err
			}
			results = append(results, res)
			for k, v := range res.Vars {
				vars[k] = v
			}
		}
		return results, nil
	}
	reg = internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          runner,
		DynamicIncludeResolver: resolver,
		PinRecorder:            pinRecorder,
		MaxIncludeDepth:        maxDepth,
	})
	return reg
}

func lockedDynamicIncludeFromPin(pin internalexecutor.DynamicIncludePin) schema.LockedDynamicInclude {
	return schema.LockedDynamicInclude{
		StepID:             pin.StepID,
		QualifiedNodeID:    pin.QualifiedNodeID,
		Invocation:         pin.Invocation,
		Revision:           pin.Revision,
		StructuralPath:     append([]schema.DynamicIncludeFrameIdentity(nil), pin.StructuralPath...),
		RenderedRef:        pin.RenderedRef,
		QualifiedID:        pin.QualifiedID,
		RunbookID:          pin.RunbookID,
		RunbookName:        pin.RunbookName,
		RunbookContentHash: pin.ContentHash,
		AbsPath:            pin.AbsPath,
		PackageName:        pin.PackageName,
		PackageVersion:     pin.PackageVersion,
		FileDigest:         pin.FileDigest,
		PackageDigest:      pin.PackageDigest,
		ExecutableClosure:  pin.ExecutableClosure,
		ResolvedInputs:     pin.ResolvedInputs,
		ResolvedGovernance: pin.ResolvedGovernance,
	}
}

func buildCatalogFromSpecsICM(t *testing.T, specs []icmPackageSpec) (*pkgcatalog.Catalog, map[string]string) {
	t.Helper()
	ws := t.TempDir()
	paths := make(map[string]string)
	requires := make([]*schema.PackageRequirement, 0, len(specs))
	for _, spec := range specs {
		pkgRoot := filepath.Join(ws, spec.RelDir)
		var manifest strings.Builder
		manifest.WriteString("apiVersion: yawr.tool-package/v1\n")
		manifest.WriteString("meta:\n")
		manifest.WriteString("  name: " + spec.Name + "\n")
		manifest.WriteString("  version: \"" + spec.Version + "\"\n")
		manifest.WriteString("exports:\n")
		manifest.WriteString("  runbooks:\n")
		for _, export := range spec.Exports {
			manifest.WriteString("    - id: " + export.ID + "\n")
			manifest.WriteString("      path: " + export.RelPath + "\n")
			absPath := filepath.Join(pkgRoot, filepath.FromSlash(export.RelPath))
			writeFile(t, absPath, export.Content)
			paths[spec.Name+"/"+export.ID] = absPath
		}
		writeFile(t, filepath.Join(pkgRoot, "yawr-package.yaml"), manifest.String())
		requires = append(requires, &schema.PackageRequirement{Package: spec.Name, Version: spec.Version, Path: spec.RelDir})
	}

	cat, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{
		WorkspaceRoot:   ws,
		ProjectRequires: requires,
	})
	if len(errs) > 0 {
		t.Fatalf("catalog build: %v", errs)
	}
	return cat, paths
}

func TestICM_HappyPath(t *testing.T) {
	catalog, childPath := buildICMTestCatalog(t)
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	pinRecorder := internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
		pins = append(pins, p)
	})
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder:            pinRecorder,
	})

	var events []icmCapturedEvent
	ctx := internalexecutor.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		if kind == string(tracepkg.EventKindIncludeResolved) {
			events = append(events, icmCapturedEvent{kind: kind, payload: payload})
		}
	})

	res, err := reg.Lookup("include").Execute(ctx, newICMIncludeStep("contoso-tsgs/tsg-network-resolve", ""), map[string]any{
		"icm_id":           "INC001",
		"suggested_tsg_id": "contoso-tsgs/tsg-network-resolve",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("status: got %s, want %s", res.Status, engine.StepStatusCompleted)
	}

	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %d", len(pins))
	}
	pin := pins[0]
	if pin.QualifiedID != "contoso-tsgs/tsg-network-resolve" {
		t.Fatalf("QualifiedID: got %q", pin.QualifiedID)
	}
	if pin.PackageName != "contoso-tsgs" || pin.PackageVersion != "1.0.0" {
		t.Fatalf("package pin mismatch: %+v", pin)
	}
	if pin.FileDigest == "" {
		t.Fatal("expected non-empty file digest")
	}
	assertICMSamePath(t, pin.AbsPath, childPath)

	if len(events) != 1 {
		t.Fatalf("expected 1 include/resolved event, got %d", len(events))
	}
	payload := events[0].payload
	if payload["qualified_id"] != "contoso-tsgs/tsg-network-resolve" {
		t.Fatalf("qualified_id: got %v", payload["qualified_id"])
	}
	if payload["package_name"] != "contoso-tsgs" {
		t.Fatalf("package_name: got %v", payload["package_name"])
	}
	gotAbsPath, ok := payload["abs_path"].(string)
	if !ok {
		t.Fatalf("abs_path payload type: got %T", payload["abs_path"])
	}
	assertICMSamePath(t, gotAbsPath, childPath)
	if got, _ := payload["file_digest"].(string); got == "" {
		t.Fatal("expected non-empty file_digest in event payload")
	}
}

func TestICM_TSGNotFound_OnNotFoundContinue(t *testing.T) {
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	catalog, errs := pkgcatalog.Build(pkgcatalog.BuildOptions{WorkspaceRoot: t.TempDir()})
	if len(errs) > 0 {
		t.Fatalf("catalog build: %v", errs)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder: internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
			pins = append(pins, p)
		}),
	})

	var resolvedEvents []icmCapturedEvent
	var notFoundEvents []icmCapturedEvent
	ctx := internalexecutor.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		switch kind {
		case string(tracepkg.EventKindIncludeResolved):
			resolvedEvents = append(resolvedEvents, icmCapturedEvent{kind: kind, payload: payload})
		case string(tracepkg.EventKindIncludeNotFound):
			notFoundEvents = append(notFoundEvents, icmCapturedEvent{kind: kind, payload: payload})
		}
	})

	res, err := reg.Lookup("include").Execute(ctx, newICMIncludeStep("tsg-unknown", schema.OnNotFoundContinue), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusSkipped {
		t.Fatalf("status: got %s, want %s", res.Status, engine.StepStatusSkipped)
	}
	found, ok := res.Vars["runbook_found"].(bool)
	if !ok || found {
		t.Fatalf("runbook_found: got %T(%v), want bool(false)", res.Vars["runbook_found"], res.Vars["runbook_found"])
	}
	// A downstream `when: "${runbook_found} == false"` condition consumes this
	// as a real bool, so the degraded branch would evaluate true.

	if len(notFoundEvents) != 1 {
		t.Fatalf("expected 1 include/notFound event, got %d", len(notFoundEvents))
	}
	if len(resolvedEvents) != 0 {
		t.Fatalf("expected no include/resolved events, got %d", len(resolvedEvents))
	}
	if len(pins) != 0 {
		t.Fatalf("expected no pins, got %d", len(pins))
	}
}

func TestICM_OperatorDeclines_WhenConditionSkipsResolution(t *testing.T) {
	catalog, _ := buildICMTestCatalog(t)
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := &trackingResolver{inner: adapter.NewCatalogIncludeResolver(catalog, parser)}

	var pins []internalexecutor.DynamicIncludePin
	executors, shutdown := newDryRunExecutorsICM(t, resolver, func(p internalexecutor.DynamicIncludePin) {
		pins = append(pins, p)
	})
	defer shutdown()

	var resolvedEvents []icmCapturedEvent
	ctx := internalexecutor.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		if kind == string(tracepkg.EventKindIncludeResolved) {
			resolvedEvents = append(resolvedEvents, icmCapturedEvent{kind: kind, payload: payload})
		}
	})

	res, err := executors.Lookup("include").Execute(ctx, newICMIncludeStep("contoso-tsgs/tsg-network-resolve", ""), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("status: got %s, want %s", res.Status, engine.StepStatusCompleted)
	}
	if resolver.callCount != 0 {
		t.Fatalf("resolver should not be called in dry-run bypass, got %d calls", resolver.callCount)
	}
	if len(resolvedEvents) != 0 {
		t.Fatalf("expected no include/resolved events, got %d", len(resolvedEvents))
	}
	if len(pins) != 0 {
		t.Fatalf("expected no pins, got %d", len(pins))
	}
}

func TestICM_DryRun_NoResolution(t *testing.T) {
	catalog, _ := buildICMTestCatalog(t)
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := &trackingResolver{inner: adapter.NewCatalogIncludeResolver(catalog, parser)}

	var pins []internalexecutor.DynamicIncludePin
	executors, shutdown := newDryRunExecutorsICM(t, resolver, func(p internalexecutor.DynamicIncludePin) {
		pins = append(pins, p)
	})
	defer shutdown()

	var resolvedEvents []icmCapturedEvent
	ctx := internalexecutor.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		if kind == string(tracepkg.EventKindIncludeResolved) {
			resolvedEvents = append(resolvedEvents, icmCapturedEvent{kind: kind, payload: payload})
		}
	})

	res, err := executors.Lookup("include").Execute(ctx, newICMIncludeStep("contoso-tsgs/tsg-network-resolve", ""), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != engine.StepStatusCompleted {
		t.Fatalf("status: got %s, want %s", res.Status, engine.StepStatusCompleted)
	}
	if got, _ := res.Output["dry_run"].(bool); !got {
		t.Fatalf("dry_run output: got %v", res.Output["dry_run"])
	}
	if got := res.Output["would_execute"]; got != "include" {
		t.Fatalf("would_execute: got %v, want include", got)
	}
	if resolver.callCount != 0 {
		t.Fatalf("resolver should not be called in dry-run mode, got %d calls", resolver.callCount)
	}
	if len(resolvedEvents) != 0 {
		t.Fatalf("expected no include/resolved events, got %d", len(resolvedEvents))
	}
	if len(pins) != 0 {
		t.Fatalf("expected no pins, got %d", len(pins))
	}
}

func TestICM_Adversarial_DINC003_FatalWithContinue(t *testing.T) {
	catalog, _ := buildCatalogFromSpecsICM(t, []icmPackageSpec{
		{
			RelDir:  "pkg-a",
			Name:    "pkg-a",
			Version: "1.0.0",
			Exports: []icmRunbookExport{{
				ID:      "tsg-disk-pressure",
				RelPath: "tsg.runbook.yaml",
				Content: "apiVersion: yawr.runbook/v1\nid: tsg-disk-pressure\nname: TSG\nflow: []\n",
			}},
		},
		{
			RelDir:  "pkg-b",
			Name:    "pkg-b",
			Version: "1.0.0",
			Exports: []icmRunbookExport{{
				ID:      "tsg-disk-pressure",
				RelPath: "tsg.runbook.yaml",
				Content: "apiVersion: yawr.runbook/v1\nid: tsg-disk-pressure\nname: TSG\nflow: []\n",
			}},
		},
	})
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder: internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
			pins = append(pins, p)
		}),
	})

	var resolvedEvents []icmCapturedEvent
	ctx := internalexecutor.WithEventEmitter(context.Background(), func(kind string, payload map[string]any) {
		if kind == string(tracepkg.EventKindIncludeResolved) {
			resolvedEvents = append(resolvedEvents, icmCapturedEvent{kind: kind, payload: payload})
		}
	})

	_, err = reg.Lookup("include").Execute(ctx, newICMIncludeStep("tsg-disk-pressure", schema.OnNotFoundContinue), nil)
	if err == nil {
		t.Fatal("expected DINC-003 error")
	}
	assertICMErrCode(t, err, "DINC-003")
	if len(pins) != 0 {
		t.Fatalf("expected no pins, got %d", len(pins))
	}
	if len(resolvedEvents) != 0 {
		t.Fatalf("expected no include/resolved events, got %d", len(resolvedEvents))
	}
}

func TestICM_Adversarial_DINC013_FatalWithContinue(t *testing.T) {
	catalog, childPath := buildICMTestCatalog(t)
	if err := os.Remove(childPath); err != nil {
		t.Fatalf("Remove(%s): %v", childPath, err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder: internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
			pins = append(pins, p)
		}),
	})

	_, err = reg.Lookup("include").Execute(context.Background(), newICMIncludeStep("contoso-tsgs/tsg-network-resolve", schema.OnNotFoundContinue), nil)
	if err == nil {
		t.Fatal("expected DINC-013 error")
	}
	assertICMErrCode(t, err, "DINC-013")
	if len(pins) != 0 {
		t.Fatalf("expected no pins, got %d", len(pins))
	}
}

func TestICM_Adversarial_CycleDetection(t *testing.T) {
	catalog, _ := buildCatalogFromSpecsICM(t, []icmPackageSpec{{
		RelDir:  "cycler",
		Name:    "cycler-pkg",
		Version: "1.0.0",
		Exports: []icmRunbookExport{{
			ID:      "cycler",
			RelPath: "runbooks/cycler.runbook.yaml",
			Content: "apiVersion: yawr.runbook/v1\nid: cycler\nname: Cycler\nflow:\n  - step:\n      id: self-include\n      type: include\n      include:\n        runbook_ref: \"cycler-pkg/cycler\"\n        resolve_from: catalog\n",
		}},
	}})
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)
	reg := newRecursiveIncludeRegistryICM(resolver, nil, 10)

	_, err = reg.Lookup("include").Execute(context.Background(), newICMIncludeStep("cycler-pkg/cycler", ""), nil)
	if err == nil {
		t.Fatal("expected DINC-004 error")
	}
	assertICMErrCode(t, err, "DINC-004")
}

func TestICM_Adversarial_MaxDepth(t *testing.T) {
	catalog, _ := buildCatalogFromSpecsICM(t, []icmPackageSpec{{
		RelDir:  "depth",
		Name:    "depth-pkg",
		Version: "1.0.0",
		Exports: []icmRunbookExport{
			{
				ID:      "a",
				RelPath: "runbooks/a.runbook.yaml",
				Content: "apiVersion: yawr.runbook/v1\nid: a\nname: A\nflow:\n  - step:\n      id: call-b\n      type: include\n      include:\n        runbook_ref: \"depth-pkg/b\"\n        resolve_from: catalog\n",
			},
			{
				ID:      "b",
				RelPath: "runbooks/b.runbook.yaml",
				Content: "apiVersion: yawr.runbook/v1\nid: b\nname: B\nflow: []\n",
			},
		},
	}})
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)
	reg := newRecursiveIncludeRegistryICM(resolver, nil, 1)

	_, err = reg.Lookup("include").Execute(context.Background(), newICMIncludeStep("depth-pkg/a", ""), nil)
	if err == nil {
		t.Fatal("expected DINC-005 error")
	}
	assertICMErrCode(t, err, "DINC-005")
}

func TestICM_Adversarial_ResumeDrift_PinCapturedByRealExecutor(t *testing.T) {
	catalog, childPath := buildICMTestCatalog(t)
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder: internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
			pins = append(pins, p)
		}),
	})

	_, err = reg.Lookup("include").Execute(context.Background(), newICMIncludeStep("contoso-tsgs/tsg-network-resolve", ""), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %d", len(pins))
	}
	pin := pins[0]
	wantDigest, err := dincFileDigestSHA256(childPath)
	if err != nil {
		t.Fatalf("dincFileDigestSHA256: %v", err)
	}
	if pin.FileDigest != "sha256:"+wantDigest {
		t.Fatalf("pin digest: got %q, want %q", pin.FileDigest, "sha256:"+wantDigest)
	}

	mutated := "apiVersion: yawr.runbook/v1\nid: tsg-network-resolve\nname: TSG Network Resolve Mutated\nflow: []\n"
	if err := os.WriteFile(childPath, []byte(mutated), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	handle := &fakeDriftHandle{state: engine.RunState{
		Plan: &engine.ExecutionPlan{
			Metadata: engine.PlanMetadata{
				DynamicIncludes: []schema.LockedDynamicInclude{lockedDynamicIncludeFromPin(pin)},
			},
		},
	}}
	if err := checkResumeDynamicIncludeDrift(context.Background(), engine.EngineConfig{}, handle, "run-1", false); err == nil {
		t.Fatal("expected DINC-012 error")
	} else {
		assertICMErrCode(t, err, "DINC-012")
	}
}

func TestICM_Adversarial_ReplayDeterminism_PinBasedLoader(t *testing.T) {
	catalog, childPath := buildICMTestCatalog(t)
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	resolver := adapter.NewCatalogIncludeResolver(catalog, parser)

	var pins []internalexecutor.DynamicIncludePin
	reg := internalexecutor.NewDefaultRegistry(internalexecutor.RegistryConfig{
		SubStepRunner:          noopSubStepRunnerICM,
		DynamicIncludeResolver: resolver,
		PinRecorder: internalexecutor.PinRecorder(func(p internalexecutor.DynamicIncludePin) {
			pins = append(pins, p)
		}),
	})

	_, err = reg.Lookup("include").Execute(context.Background(), newICMIncludeStep("contoso-tsgs/tsg-network-resolve", ""), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("expected 1 pin, got %d", len(pins))
	}
	pin := pins[0]

	mutatedPath := filepath.Join(filepath.Dir(childPath), "tsg-network-resolve-v2.runbook.yaml")
	writeFile(t, mutatedPath, "apiVersion: yawr.runbook/v1\nid: tsg-network-resolve\nname: catalog mutated\nflow: []\n")

	base := &capturingFileLoader{}
	loader := internalreplay.NewPinBasedIncludeLoader(base, map[string]schema.LockedDynamicInclude{
		pin.AbsPath: lockedDynamicIncludeFromPin(pin),
	}, nil, "run-1")

	loaded, err := loader.Load(context.Background(), pin.AbsPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(pin.ExecutableClosure) == 0 || loaded == nil || len(loaded.Flow) != 0 {
		t.Fatalf("restored pinned closure = %#v, pin=%#v", loaded, pin)
	}
	if base.loadedPath != "" || base.loadedContent != "" {
		t.Fatalf("embedded closure delegated to filesystem: path=%q content=%q", base.loadedPath, base.loadedContent)
	}
}
