package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgdrift"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// checkResumePackageDrift implements the Package Resumption Contract
// (design/yawr/sections/13-evidence-tracing-resumption.tex §Package
// Resumption Contract) at the actual `yawr run --resume` boundary: it
// recomputes the current package/catalog digests exactly the way a fresh
// run would (same requires:/package-map/project-config resolution as the
// non-resume branch of runWithMode) and compares them against the digests
// frozen into the resumed plan's Metadata at the original Start time
// (cmd/yawr/run.go, builtCatalog wiring). A mismatch is a hard PKG-009
// refusal unless allowDrift is set, in which case the bypass is recorded
// as a governance/packageDriftAccepted trace event -- never a silent
// bypass -- via pkg/pkgdrift.Evaluate.
//
// Returns nil when there is nothing to check (the resumed plan never
// resolved a package catalog, e.g. a runbook with no requires:/toolRefs:)
// or when digests match / drift was explicitly accepted.
func checkResumePackageDrift(
	ctx context.Context,
	ecfg engine.EngineConfig,
	parserImpl parser.Parser,
	handle engine.RunHandle,
	resumeID string,
	packageMapPath string,
	allowDrift bool,
) error {
	state := handle.State()
	if state.Plan == nil || state.Plan.Metadata.CatalogDigest == "" {
		// This run's plan never resolved a package catalog (no
		// requires:/toolRefs: at plan time) -- nothing to drift-check.
		return nil
	}

	parsed, err := parserImpl.Parse(ctx, state.Plan.RunbookPath)
	if err != nil {
		return fmt.Errorf("resume: re-parse runbook for package drift check: %w", err)
	}
	if parsed.Runbook == nil {
		return nil
	}

	workspaceRoot, wderr := os.Getwd()
	if wderr != nil {
		return wderr
	}
	projCfg, cfgErr := loadProjectConfig(workspaceRoot)
	if cfgErr != nil {
		return cfgErr
	}
	var mergedRequires []*schema.PackageRequirement
	var mergedToolPaths []string
	if projCfg != nil {
		mergedRequires = projCfg.Requires
		mergedToolPaths = projCfg.ToolPaths
	}
	if packageMapPath != "" {
		pmCfg, pmErr := loadPackageMap(packageMapPath)
		if pmErr != nil {
			return pmErr
		}
		if pmCfg != nil {
			merged, _ := mergePackageBindings(mergedRequires, pmCfg.Requires)
			mergedRequires = merged
			mergedToolPaths = mergeToolPaths(mergedToolPaths, pmCfg.ToolPaths)
		}
	}

	catOpts := adapter.PackageCatalogOptions{
		WorkspaceRoot:    workspaceRoot,
		Builtins:         internaltool.NewBuiltinRegistry().All(),
		ProjectRequires:  mergedRequires,
		ProjectToolPaths: mergedToolPaths,
	}
	cat, catErrs := adapter.BuildPackageCatalog(catOpts, state.Plan.RunbookPath, parsed.Runbook.Requires)
	fatalCatErrs, _ := errkit.SplitWarnings(catErrs)
	if len(fatalCatErrs) > 0 {
		return fatalCatErrs[0]
	}

	// §7.5 rule 5 (B4): the manifest's recorded PackageDigests is the
	// AUTHORITATIVE set, not merely a filter over whichever packages
	// happen to resolve today. Iterate the UNION of recorded and current
	// package names: a name recorded at plan time with no corresponding
	// resolved package on resume is a hard PKG-001 (missing package),
	// named explicitly -- never silently folded into the weaker PKG-009
	// catalog-digest mismatch. A resolved package absent from the
	// manifest (newly appearing) is PKG-009 drift, exactly like a digest
	// mismatch on a package both sides agree existed.
	currentByName := make(map[string]string, len(cat.Packages))
	for _, p := range cat.Packages {
		currentByName[p.Name] = p.Digest
	}
	var missing []string
	for name := range state.Plan.Metadata.PackageDigests {
		if _, ok := currentByName[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return errkit.New("PKG-001", fmt.Sprintf(
			"resume: package(s) recorded at plan time are no longer resolvable: %s", strings.Join(missing, ", ")))
	}

	pairs := []pkgdrift.DigestPair{{
		Name:     "*",
		Expected: state.Plan.Metadata.CatalogDigest,
		Actual:   cat.CatalogDigest(),
	}}
	for name, digest := range currentByName {
		expected, tracked := state.Plan.Metadata.PackageDigests[name]
		if !tracked {
			// A package resolved now but not recorded at plan time: not
			// missing (that's PKG-001 above), but still a genuine drift
			// from what the plan froze -- surfaced as PKG-009 alongside
			// any digest mismatch, per the catalog-digest comparison's own
			// "*" pair above (which would already differ in this case).
			pairs = append(pairs, pkgdrift.DigestPair{Name: name, Expected: "", Actual: digest})
			continue
		}
		pairs = append(pairs, pkgdrift.DigestPair{Name: name, Expected: expected, Actual: digest})
	}
	sort.Slice(pairs[1:], func(i, j int) bool { return pairs[1+i].Name < pairs[1+j].Name })

	operator := resumeOperator()
	decision := pkgdrift.Evaluate(pkgdrift.ModeResume, pairs, allowDrift, operator)
	if !decision.Allowed {
		return decision.Err
	}
	for _, ev := range decision.DriftAcceptedEvents {
		payload, merr := json.Marshal(trace.GovernancePackageDriftAcceptedPayload{
			ExpectedCatalogDigest: ev.ExpectedCatalogDigest,
			ActualCatalogDigest:   ev.ActualCatalogDigest,
			Operator:              ev.Operator,
		})
		if merr != nil {
			return merr
		}
		if ecfg.TraceWriter != nil {
			if werr := ecfg.TraceWriter.Append(trace.TraceEvent{
				RunID:   resumeID,
				Kind:    trace.EventKindGovernancePackageDriftAccepted,
				Payload: payload,
			}); werr != nil {
				return werr
			}
		}
	}
	return nil
}

func resumeOperator() string {
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return "unknown"
}

// checkResumeDynamicIncludeDrift implements the Dynamic Include Resumption
// Contract (barbara-dynamic-include-contract.md §8.4) at the `yawr run
// --resume` boundary: it verifies the SHA-256 file digest of each pinned
// dynamic include against the current on-disk file. A mismatch is a hard
// DINC-012 refusal unless allowDrift is set, in which case the bypass is
// recorded as a governance/packageDriftAccepted trace event — never a
// silent bypass (B-12: no new flag; --allow-package-drift covers this).
//
// Returns nil when the plan has no dynamic include pins, all pins match,
// or drift was explicitly accepted.
func checkResumeDynamicIncludeDrift(
	ctx context.Context,
	ecfg engine.EngineConfig,
	handle engine.RunHandle,
	resumeID string,
	allowDrift bool,
) error {
	_ = ctx
	state := handle.State()
	if state.Plan == nil || len(state.Plan.Metadata.DynamicIncludes) == 0 {
		return nil
	}

	type mismatch struct {
		pin    schema.LockedDynamicInclude
		actual string // empty if the file has vanished
	}
	var mismatches []mismatch

	for _, pin := range state.Plan.Metadata.DynamicIncludes {
		actual, err := dincFileDigestSHA256(pin.AbsPath)
		if err != nil {
			// File vanished or is unreadable — always a hard mismatch.
			mismatches = append(mismatches, mismatch{pin: pin, actual: ""})
			continue
		}
		if actual != pin.FileDigest {
			mismatches = append(mismatches, mismatch{pin: pin, actual: actual})
		}
	}

	if len(mismatches) == 0 {
		return nil
	}

	if !allowDrift {
		var parts []string
		for _, m := range mismatches {
			if m.actual == "" {
				parts = append(parts, fmt.Sprintf("%s (%s): file vanished", m.pin.StepID, m.pin.QualifiedID))
			} else {
				parts = append(parts, fmt.Sprintf("%s (%s): expected %s got %s",
					m.pin.StepID, m.pin.QualifiedID, m.pin.FileDigest, m.actual))
			}
		}
		return errkit.New("DINC-012",
			"resume: dynamic include pin digest mismatch: "+strings.Join(parts, "; ")+
				" (pass --allow-package-drift to override; the run stays resumable)")
	}

	// Drift accepted — emit one audit record per mismatch. Never silent.
	operator := resumeOperator()
	for _, m := range mismatches {
		payload, merr := json.Marshal(trace.GovernancePackageDriftAcceptedPayload{
			ExpectedCatalogDigest: m.pin.FileDigest,
			ActualCatalogDigest:   m.actual,
			Operator:              operator,
		})
		if merr != nil {
			return merr
		}
		if ecfg.TraceWriter != nil {
			if werr := ecfg.TraceWriter.Append(trace.TraceEvent{
				RunID:   resumeID,
				Kind:    trace.EventKindGovernancePackageDriftAccepted,
				Payload: payload,
			}); werr != nil {
				return werr
			}
		}
	}
	return nil
}

// dincFileDigestSHA256 returns the lower-case hex SHA-256 digest of the
// file at path. Used by checkResumeDynamicIncludeDrift for pin verification.
func dincFileDigestSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
