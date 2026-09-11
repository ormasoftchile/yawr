package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// TestCheckResumePackageDrift_RemovedPackage_PKG001 is a B4 regression test
// (Barbara's gate review): a package recorded in the manifest
// (plan.Metadata.PackageDigests) but absent from the current resolution
// (e.g. its requires: entry was deleted from .yawr/config.yaml between
// plan-time and resume) MUST raise PKG-001 naming the missing package --
// not the generic PKG-009 digest-mismatch code, and not silently pass.
func TestCheckResumePackageDrift_RemovedPackage_PKG001(t *testing.T) {
	dir, cat, plan := setupDriftTestFixture(t)
	ctx := context.Background()
	ecfg := engine.EngineConfig{TraceWriter: noopTraceWriter{}}

	plan.Metadata.CatalogDigest = cat.CatalogDigest()
	// Simulate the package having been removed from project config since
	// the plan was recorded: config.yaml no longer requires it, so the
	// rebuilt catalog resolves zero packages, while
	// plan.Metadata.PackageDigests still names it.
	writeFile(t, filepath.Join(dir, ".yawr", "config.yaml"), "apiVersion: yawr.config/v1\n")

	handle := &fakeResumeHandle{state: engine.RunState{RunID: "run-1", Plan: plan}}

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err := checkResumePackageDrift(ctx, ecfg, mustParser(t), handle, "run-1", "", false)
	if err == nil {
		t.Fatalf("expected PKG-001 error for removed package, got nil")
	}
	if !strings.Contains(err.Error(), "PKG-001") {
		t.Fatalf("expected PKG-001 in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "acme.drift-tools") {
		t.Fatalf("expected removed package name %q in error, got: %v", "acme.drift-tools", err)
	}
}

// TestCheckResumePackageDrift_AddedPackage_PKG009 is a B4 regression test:
// a package resolved by the current catalog but absent from the manifest's
// recorded PackageDigests (e.g. a requires: entry was added since the plan
// was recorded) is not simply ignored -- it forces a digest mismatch
// (PKG-009), since the manifest cannot vouch for a package it never saw.
func TestCheckResumePackageDrift_AddedPackage_PKG009(t *testing.T) {
	dir, cat, plan := setupDriftTestFixture(t)
	ctx := context.Background()
	ecfg := engine.EngineConfig{TraceWriter: noopTraceWriter{}}

	plan.Metadata.CatalogDigest = cat.CatalogDigest()
	// Simulate the package having been added to project config since the
	// plan was recorded: the manifest's PackageDigests map (frozen at
	// plan time) never saw it.
	plan.Metadata.PackageDigests = map[string]string{}

	handle := &fakeResumeHandle{state: engine.RunState{RunID: "run-1", Plan: plan}}

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	err := checkResumePackageDrift(ctx, ecfg, mustParser(t), handle, "run-1", "", false)
	if err == nil {
		t.Fatalf("expected PKG-009 error for added/untracked package, got nil")
	}
	if !strings.Contains(err.Error(), "PKG-009") {
		t.Fatalf("expected PKG-009 in error, got: %v", err)
	}
}

// noopTraceWriter discards every event; used by tests that don't inspect
// trace output.
type noopTraceWriter struct{}

func (noopTraceWriter) Append(trace.TraceEvent) error { return nil }
func (noopTraceWriter) Close() error                  { return nil }
