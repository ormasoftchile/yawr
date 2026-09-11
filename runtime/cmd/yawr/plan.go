package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/errkit"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

// runPlan implements `yawr plan [flags] <runbook>`.
//
// Static, side-effect-free analysis. Does NOT execute anything, does NOT
// authenticate, does NOT touch the network. It answers the counterparty's
// driving question: "is this runbook configured to run in this context?"
// before anyone runs it.
//
// Exit codes:
//
//	0 — plan passed; Tier 0 preflight clean
//	2 — plan failed (Tier 0 violation, catalog error, parse error)
//	3 — runtime error (OS, I/O)
//
// Composition rules (unchanged and contractual):
//   - --package-map wins at YAML selection (which tool definition/package binds).
//   - --profile parameterizes execution after selection.
//   - They compose, never compete.
//   - plan.Tools (post-catalog, post-package-map) is the source of truth.
func runPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	outputFormat := fs.String("output", outputText, "Output format: text, json")
	packageMapPath := fs.String("package-map", "", "Path to a package-map file (yawr.config/v1) overriding project package bindings")
	profilePath := fs.String("profile", "", "Runtime profile file (yawr.runtime-profile/v1) for Tier 0 preflight + approval analysis")
	expandMode := fs.String("expand", "", "Default expansion mode: eager, lazy, or auto")
	showProfileDirs := fs.String("show-profiles", "", "Comma-separated directories to scan for runtime profiles and report which ones provide a complete binding for this runbook")

	flagArgs, runbookPath, extraArgs := splitPlanArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		if err == flag.ErrHelp {
			return exitSuccess
		}
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}
	if runbookPath == "" || len(extraArgs) > 0 {
		fmt.Fprintln(os.Stderr, "usage: yawr plan [--profile <path>] [--package-map <path>] [--show-profiles <dirs>] [--output text|json] <runbook-path>")
		return exitValidation
	}
	if *outputFormat != outputText && *outputFormat != outputJSON {
		fmt.Fprintln(os.Stderr, "invalid output format; use text or json")
		return exitValidation
	}

	expandDefault, err := expand.Parse(*expandMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}

	// Load and validate the runtime profile.
	var runtimeProfile *schema.RuntimeProfile
	if *profilePath != "" {
		loaded, profErr := schema.ParseProfileFile(*profilePath)
		if profErr != nil {
			fmt.Fprintln(os.Stderr, profErr)
			return exitValidation
		}
		runtimeProfile = loaded
	}

	ctx := context.Background()

	plat := platform.Real()
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}

	// plannerToolRegistry is the schema-level registry the planner uses for
	// tool/action lookups. For plan we don't need a runtime registry at all.
	registry, err := newToolRegistry(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}

	parsed, err := parserImpl.Parse(ctx, runbookPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitValidation
	}
	for _, w := range parsed.Warnings {
		fmt.Fprintf(os.Stderr, "yawr: warning: %s: %s\n", w.Field, w.Message)
	}

	// Catalog building + toolRef binding.
	// Duplicated from run.go (rather than extracted) to avoid destabilising
	// the run path. Only the planner-schema registry is populated here;
	// no runtime registry is needed for plan-only analysis.
	var builtCatalog *pkgcatalog.Catalog
	if parsed.Runbook != nil {
		workspaceRoot, wderr := os.Getwd()
		if wderr != nil {
			fmt.Fprintln(os.Stderr, wderr)
			return exitRuntime
		}
		projCfg, cfgErr := loadProjectConfig(workspaceRoot)
		if cfgErr != nil {
			fmt.Fprintln(os.Stderr, cfgErr)
			return exitValidation
		}
		var projectRequires []*schema.PackageRequirement
		var projectToolPaths []string
		if projCfg != nil {
			projectRequires = projCfg.Requires
			projectToolPaths = projCfg.ToolPaths
		}
		var overrideRequires []*schema.PackageRequirement
		var overrideToolPaths []string
		if *packageMapPath != "" {
			pmCfg, pmErr := loadPackageMap(*packageMapPath)
			if pmErr != nil {
				fmt.Fprintln(os.Stderr, pmErr)
				return exitValidation
			}
			if pmCfg != nil {
				overrideRequires = pmCfg.Requires
				overrideToolPaths = pmCfg.ToolPaths
			}
		}
		mergedRequires, _ := mergePackageBindings(projectRequires, overrideRequires)
		mergedToolPaths := mergeToolPaths(projectToolPaths, overrideToolPaths)

		catOpts := adapter.PackageCatalogOptions{
			WorkspaceRoot:    workspaceRoot,
			Builtins:         internaltool.NewBuiltinRegistry().All(),
			ProjectRequires:  mergedRequires,
			ProjectToolPaths: mergedToolPaths,
		}
		cat, catErrs := adapter.BuildPackageCatalog(catOpts, runbookPath, parsed.Runbook.Requires)
		fatalCatErrs, warnCatErrs := errkit.SplitWarnings(catErrs)
		if len(fatalCatErrs) > 0 {
			for _, e := range fatalCatErrs {
				fmt.Fprintln(os.Stderr, e)
			}
			return exitValidation
		}
		for _, e := range warnCatErrs {
			fmt.Fprintln(os.Stderr, e)
		}
		builtCatalog = cat

		if len(parsed.Runbook.ToolRefs) > 0 {
			defs, bindErrs := adapter.ResolveToolRefsViaCatalog(cat, runbookPath, parsed.Runbook.ToolRefs)
			fatalBind, warnBind := errkit.SplitWarnings(bindErrs)
			if len(fatalBind) > 0 {
				for _, e := range fatalBind {
					fmt.Fprintln(os.Stderr, e)
				}
				return exitValidation
			}
			for _, e := range warnBind {
				fmt.Fprintln(os.Stderr, e)
			}
			// Populate the planner schema registry only — no runtime registry
			// is needed for static analysis.
			for _, def := range defs {
				schemaDef := schemaToolDefFromRuntime(def)
				for action := range schemaDef.Actions {
					registry.tools[schemaDef.Name+"/"+action] = schemaDef
				}
			}
		}
	}

	// penv carries everything needed to re-run Plan() with different profiles,
	// enabling both the --show-profiles scan and the config/no-binding-for-profile
	// alternatives note without repeating catalog/registry setup.
	penv := &plannerEnv{
		ctx:    ctx,
		parsed: parsed,
		reg:    registry,
		expand: expandDefault,
		loader: &fileRunbookLoader{parser: parserImpl},
	}

	// ── --show-profiles mode ──────────────────────────────────────────────
	// Pure scan: which profiles in the given directories provide a complete
	// binding for all toolRefs in this runbook? Always exits 0 (zero matches
	// is a valid informative answer, not a failure).
	if *showProfileDirs != "" {
		searchDirs := splitProfileDirs(*showProfileDirs)
		return runShowProfiles(ctx, penv, runbookPath, searchDirs, *outputFormat)
	}

	// ── Primary plan run ──────────────────────────────────────────────────
	routeDocument, routeHashErr := (&graphdoc.Builder{Loader: &cliLoader{p: parserImpl}, Recurse: true}).Build(ctx, parsed)
	if routeHashErr != nil {
		fmt.Fprintln(os.Stderr, routeHashErr)
		return exitValidation
	}
	plan, planErr := penv.planWithProfile(runtimeProfile)
	if planErr != nil {
		// Collect alternative profiles that would work, for the
		// config/no-binding-for-profile diagnostic.
		var altSearchDirs []string
		if *profilePath != "" {
			// Auto-search the directory that contains the selected profile.
			altSearchDirs = []string{filepath.Dir(*profilePath)}
		}
		alts := findCompatibleProfiles(ctx, penv, altSearchDirs)

		if *outputFormat == outputJSON {
			renderPlanErrorJSON(os.Stdout, planErr, alts)
		} else {
			renderPlanErrorText(os.Stderr, planErr, alts)
		}
		return exitValidation
	}

	// Plan succeeded — render the full output.
	routeTestHash, routeHashErr := routeTestPlanHash(routeDocument.Hash, plan, builtCatalog, runtimeProfile)
	if routeHashErr != nil {
		fmt.Fprintln(os.Stderr, routeHashErr)
		return exitValidation
	}
	out := assemblePlanOutput(plan, runtimeProfile, builtCatalog)
	out.RouteTestHash = routeTestHash
	if *outputFormat == outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		renderPlanOutputText(os.Stdout, out)
	}
	return exitSuccess
}

// ── Output types ─────────────────────────────────────────────────────────────

// planOutput is the structured result of `yawr plan`.
type planOutput struct {
	Runbook       string            `json:"runbook"`
	RouteTestHash string            `json:"route_test_hash"`
	Profile       *planProfileJSON  `json:"profile,omitempty"`
	Preflight     planPreflightJSON `json:"preflight"`
	Tools         []planToolJSON    `json:"tools"`
	Actions       []planActionJSON  `json:"actions"`
}

func routeTestPlanHash(graphHash string, plan *engine.ExecutionPlan, catalog *pkgcatalog.Catalog, profile *schema.RuntimeProfile) (string, error) {
	catalogDigest := plan.Metadata.CatalogDigest
	if catalogDigest == "" {
		emptyCatalog := sha256.Sum256(nil)
		catalogDigest = fmt.Sprintf("sha256:%x", emptyCatalog)
	}
	var lockedPackages []pkgcatalog.LockedPackageInfo
	if catalog != nil {
		catalogDigest = catalog.CatalogDigest()
		lockedPackages = append([]pkgcatalog.LockedPackageInfo(nil), catalog.Packages...)
	}
	packageIdentity := struct {
		Locked  []pkgcatalog.LockedPackageInfo `json:"locked,omitempty"`
		Digests map[string]string              `json:"digests,omitempty"`
	}{Locked: lockedPackages, Digests: plan.Metadata.PackageDigests}
	encodedPackages, err := json.Marshal(packageIdentity)
	if err != nil {
		return "", fmt.Errorf("route test: encode package identity: %w", err)
	}
	packageDigestBytes := sha256.Sum256(encodedPackages)
	packageLockDigest := fmt.Sprintf("sha256:%x", packageDigestBytes)
	payload := struct {
		GraphHash          string                     `json:"graph_hash"`
		ExecutablePlanHash string                     `json:"executable_plan_hash"`
		CatalogDigest      string                     `json:"catalog_digest"`
		PackageLockDigest  string                     `json:"package_lock_digest"`
		Tools              map[string]*schema.ToolDef `json:"tools"`
		Profile            *schema.RuntimeProfile     `json:"profile,omitempty"`
	}{
		GraphHash: graphHash, ExecutablePlanHash: plan.Metadata.PlanHash,
		CatalogDigest: catalogDigest, PackageLockDigest: packageLockDigest,
		Tools: plan.Tools, Profile: profile,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("route test: encode plan identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest), nil
}

type planProfileJSON struct {
	ID         string `json:"id"`
	Context    string `json:"context"`
	Attendance string `json:"attendance"`
}

type planPreflightJSON struct {
	Pass   bool   `json:"pass"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type planToolJSON struct {
	Name      string `json:"name"`
	Package   string `json:"package,omitempty"`
	Transport string `json:"transport,omitempty"`
	Status    string `json:"status"`
}

type planActionJSON struct {
	Tool           string `json:"tool"`
	Action         string `json:"action"`
	Classification string `json:"classification"`
	Outcome        string `json:"outcome"`
	DenyReason     string `json:"deny_reason,omitempty"`
}

// planErrorJSON is the JSON envelope for a plan-time failure.
type planErrorJSON struct {
	Runbook        string            `json:"runbook,omitempty"`
	Preflight      planPreflightJSON `json:"preflight"`
	CompatibleWith []string          `json:"compatible_profiles,omitempty"`
}

// ── Assembly ──────────────────────────────────────────────────────────────────

// assemblePlanOutput builds the full planOutput from a successfully planned
// execution, the optional profile, and the optional catalog.
func assemblePlanOutput(
	plan *engine.ExecutionPlan,
	profile *schema.RuntimeProfile,
	cat *pkgcatalog.Catalog,
) planOutput {
	out := planOutput{
		Runbook:   plan.RunbookPath,
		Preflight: planPreflightJSON{Pass: true},
	}

	if profile != nil {
		out.Profile = &planProfileJSON{
			ID:         profile.ID,
			Context:    string(profile.Context),
			Attendance: string(profile.Attendance),
		}
	}

	// Tool bindings (sorted by name for deterministic output).
	toolNames := make([]string, 0, len(plan.Tools))
	for name := range plan.Tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	for _, name := range toolNames {
		def := plan.Tools[name]
		t := planToolJSON{
			Name:      name,
			Transport: resolveTransportMode(def),
			Status:    "ok",
		}
		if cat != nil {
			entries := cat.ByBare(name)
			if len(entries) > 0 {
				e := entries[len(entries)-1]
				if e.PackageName != "" {
					if e.Version != "" {
						t.Package = e.PackageName + "@" + e.Version
					} else {
						t.Package = e.PackageName
					}
				}
			}
		}
		out.Tools = append(out.Tools, t)
	}

	// Action approval consequences (sorted by tool name, then action name).
	type toolActionKey struct {
		tool   string
		action string
	}
	var keys []toolActionKey
	for _, name := range toolNames {
		def := plan.Tools[name]
		for actionName := range def.Actions {
			keys = append(keys, toolActionKey{tool: name, action: actionName})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tool != keys[j].tool {
			return keys[i].tool < keys[j].tool
		}
		return keys[i].action < keys[j].action
	})

	for _, k := range keys {
		def := plan.Tools[k.tool]
		action := def.Actions[k.action]
		classification := "missing"
		if action != nil && action.Classification != nil && *action.Classification != "" {
			classification = *action.Classification
		}
		outcome, denyReason := computeActionApprovalOutcome(profile, def.Governance, action)
		aj := planActionJSON{
			Tool:           k.tool,
			Action:         k.action,
			Classification: classification,
			Outcome:        string(outcome),
		}
		if denyReason != "" {
			aj.DenyReason = denyReason
		}
		out.Actions = append(out.Actions, aj)
	}

	return out
}

// resolveTransportMode returns the effective transport mode string from a
// schema.ToolDef. Prefers Transport.Mode (canonical) over Transport.Type.
func resolveTransportMode(def *schema.ToolDef) string {
	if def == nil {
		return ""
	}
	if def.Transport.Mode != "" {
		return def.Transport.Mode
	}
	if def.Transport.Type != "" {
		return string(def.Transport.Type)
	}
	return "stdio"
}

// ── Approval outcome computation ──────────────────────────────────────────────

// planApprovalOutcome is the static approval consequence for one action.
type planApprovalOutcome string

const (
	planOutcomeExplicitRequired planApprovalOutcome = "approval-required (explicit)"
	planOutcomeAllowed          planApprovalOutcome = "allowed"
	planOutcomeApprovalGate     planApprovalOutcome = "approval-required"
	planOutcomeDenied           planApprovalOutcome = "denied"
)

// computeActionApprovalOutcome applies the approval classification matrix.
// purely statically — no runtime, no evaluator interface, no side effects.
//
// Mirrors internal/governance/profile_evaluator.go's matrix exactly:
//
//	classification | test context | attended | unattended
//	read-only      | allow        | allow if scope.AllowRead; else deny
//	mutating       | allow        | gate     | allow if scope.AllowMutating; else deny
//	destructive    | allow        | gate     | allow if scope.AllowDestructive; else deny
//
// Does NOT import internal/governance to avoid a circular coupling.
func computeActionApprovalOutcome(
	profile *schema.RuntimeProfile,
	governance *schema.ToolGovernance,
	action *schema.ToolAction,
) (planApprovalOutcome, string) {
	if profile == nil {
		return planOutcomeAllowed, ""
	}

	if governance != nil && governance.RequiresApproval != nil && !*governance.RequiresApproval {
		return planOutcomeDenied, "requires-approval false is unsupported"
	}
	// Explicit requires-approval: true — gate always fires.
	if governance != nil && governance.RequiresApproval != nil && *governance.RequiresApproval {
		return planOutcomeExplicitRequired, ""
	}

	classification := ""
	if action != nil && action.Classification != nil {
		classification = *action.Classification
	}

	isTest := profile.Context == schema.ProfileContextTest
	isAttended := profile.Attendance == schema.ProfileAttendanceAttended
	scope := profile.Approval.Scope

	switch classification {
	case "read-only":
		if isTest || scope.AllowRead {
			return planOutcomeAllowed, ""
		}
		return planOutcomeDenied, "read-only action not permitted by profile scope (allow_read=false)"

	case "mutating":
		if isTest {
			return planOutcomeAllowed, ""
		}
		if isAttended {
			return planOutcomeApprovalGate, ""
		}
		if scope.AllowMutating {
			return planOutcomeAllowed, ""
		}
		return planOutcomeDenied, "mutating action denied in unattended context (allow_mutating=false)"

	case "destructive":
		if isTest {
			return planOutcomeAllowed, ""
		}
		if isAttended {
			return planOutcomeApprovalGate, ""
		}
		if scope.AllowDestructive {
			return planOutcomeAllowed, ""
		}
		return planOutcomeDenied, "destructive action denied in unattended context (allow_destructive=false)"

	default:
		return planOutcomeDenied, "explicit action classification is required"
	}
}

// ── Rendering ─────────────────────────────────────────────────────────────────

func renderPlanErrorText(w *os.File, err error, alts []string) {
	fmt.Fprintf(w, "config/no-binding-for-profile: %v\n", err)
	if len(alts) > 0 {
		fmt.Fprintf(w, "  note: this runbook runs correctly in profiles: %s\n", strings.Join(alts, ", "))
	} else if alts != nil {
		// alts is non-nil but empty: we searched and found nothing.
		fmt.Fprintln(w, "  note: no discovered profile provides a complete binding for this runbook")
	}
}

func renderPlanErrorJSON(w *os.File, err error, alts []string) {
	code := ""
	detail := err.Error()
	if c, ok := err.(errkit.Coder); ok {
		code = c.Code()
	}
	out := planErrorJSON{
		Preflight:      planPreflightJSON{Pass: false, Code: code, Detail: detail},
		CompatibleWith: alts,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// ── Profile scan helpers ───────────────────────────────────────────────────────

// plannerEnv carries everything needed to re-run Plan() with different
// profiles. Building catalog and registry is expensive; we do it once and
// then swap only the Profile field for each candidate.
type plannerEnv struct {
	ctx    context.Context
	parsed *parserpkg.ParsedRunbook
	reg    *plannerToolRegistry
	expand expand.Mode
	loader plannerpkg.RunbookLoader
}

// planWithProfile constructs a fresh planner with the given profile and
// runs Plan(). The catalog and registry are shared from penv.
func (penv *plannerEnv) planWithProfile(profile *schema.RuntimeProfile) (*engine.ExecutionPlan, error) {
	pl := internalplanner.New(plannerpkg.Config{
		Loader:       penv.loader,
		Tools:        penv.reg,
		ExpandPolicy: expand.Policy{Default: penv.expand},
		Profile:      profile,
	})
	return pl.Plan(penv.ctx, penv.parsed)
}

// findCompatibleProfiles scans each directory in dirs for runtime profile
// files, and returns the IDs of all profiles under which Plan() succeeds.
// Returns nil (not an empty slice) when dirs is empty, so callers can
// distinguish "no search dirs configured" from "searched and found nothing".
func findCompatibleProfiles(ctx context.Context, penv *plannerEnv, dirs []string) []string {
	_ = ctx // used via penv.ctx
	if len(dirs) == 0 {
		return nil
	}
	candidates, _ := schema.DiscoverProfiles(dirs) // best-effort: ignore discovery errors
	if len(candidates) == 0 {
		return []string{} // non-nil empty: searched but found nothing
	}
	var compatible []string
	for _, prof := range candidates {
		if _, err := penv.planWithProfile(prof); err == nil {
			compatible = append(compatible, prof.ID)
		}
	}
	if compatible == nil {
		return []string{} // searched, found profiles, none worked
	}
	sort.Strings(compatible)
	return compatible
}

// runShowProfiles is the handler for `--show-profiles`. It scans searchDirs
// for profile files, tests each one against the runbook, and reports results.
//
// Exit code is always 0 — zero matches is a valid informative answer.
func runShowProfiles(
	ctx context.Context,
	penv *plannerEnv,
	runbookPath string,
	searchDirs []string,
	outputFmt string,
) int {
	candidates, discErrs := schema.DiscoverProfiles(searchDirs)
	for _, e := range discErrs {
		fmt.Fprintln(os.Stderr, e)
	}

	type result struct {
		ID         string `json:"id"`
		Context    string `json:"context"`
		Attendance string `json:"attendance"`
		Compatible bool   `json:"compatible"`
		Error      string `json:"error,omitempty"`
	}
	var results []result
	for _, prof := range candidates {
		_, planErr := penv.planWithProfile(prof)
		r := result{
			ID:         prof.ID,
			Context:    string(prof.Context),
			Attendance: string(prof.Attendance),
			Compatible: planErr == nil,
		}
		if planErr != nil {
			r.Error = planErr.Error()
		}
		results = append(results, r)
	}

	// Collect compatible IDs for the "zero matches" message.
	var compatIDs []string
	for _, r := range results {
		if r.Compatible {
			compatIDs = append(compatIDs, r.ID)
		}
	}

	if len(compatIDs) == 0 {
		// Exit 0, empty stdout, single-line stderr.
		searchDesc := strings.Join(searchDirs, ", ")
		fmt.Fprintf(os.Stderr, "no profile in %s provides a complete binding for all toolRefs in %s\n",
			searchDesc, runbookPath)
		return exitSuccess
	}

	if outputFmt == outputJSON {
		type showOutput struct {
			Runbook    string   `json:"runbook"`
			SearchDirs []string `json:"search_dirs"`
			Profiles   []result `json:"profiles"`
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(showOutput{
			Runbook:    runbookPath,
			SearchDirs: searchDirs,
			Profiles:   results,
		})
	} else {
		fmt.Fprintf(os.Stdout, "Compatible profiles for %s:\n", runbookPath)
		idW, ctxW, attW := 16, 18, 12
		for _, r := range results {
			if l := len(r.ID) + 2; l > idW {
				idW = l
			}
			if l := len(r.Context) + 2; l > ctxW {
				ctxW = l
			}
		}
		hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %s", idW, "ID", ctxW, "CONTEXT", attW, "ATTENDANCE", "STATUS")
		fmt.Fprintln(os.Stdout, hdr)
		fmt.Fprintln(os.Stdout, "  "+strings.Repeat("─", len(hdr)-2))
		for _, r := range results {
			status := "ok"
			if !r.Compatible {
				status = "fail: " + r.Error
			}
			fmt.Fprintf(os.Stdout, "  %-*s  %-*s  %-*s  %s\n",
				idW, r.ID, ctxW, r.Context, attW, r.Attendance, status)
		}
	}
	return exitSuccess
}

// splitProfileDirs splits a comma-separated list of directory paths, trimming
// whitespace from each element.
func splitProfileDirs(s string) []string {
	var dirs []string
	for _, d := range strings.Split(s, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func renderPlanOutputText(w *os.File, r planOutput) {
	// Header.
	if r.Profile != nil {
		fmt.Fprintf(w, "Profile:    %s\n", r.Profile.ID)
		fmt.Fprintf(w, "  context:    %s\n", r.Profile.Context)
		fmt.Fprintf(w, "  attendance: %s\n", r.Profile.Attendance)
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "Profile: (none — no Tier 0 preflight checks, no approval analysis)")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Tier 0 preflight: PASS")
	fmt.Fprintln(w)

	// Tool bindings table.
	if len(r.Tools) > 0 {
		fmt.Fprintln(w, "Tool bindings:")
		nameW, pkgW, transW := 12, 20, 12
		for _, t := range r.Tools {
			if l := len(t.Name) + 2; l > nameW {
				nameW = l
			}
			pkg := t.Package
			if pkg == "" {
				pkg = "workspace"
			}
			if l := len(pkg) + 2; l > pkgW {
				pkgW = l
			}
			if l := len(t.Transport) + 2; l > transW {
				transW = l
			}
		}
		hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %s", nameW, "NAME", pkgW, "PACKAGE", transW, "TRANSPORT", "STATUS")
		fmt.Fprintln(w, hdr)
		fmt.Fprintln(w, "  "+strings.Repeat("─", len(hdr)-2))
		for _, t := range r.Tools {
			pkg := t.Package
			if pkg == "" {
				pkg = "workspace"
			}
			fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %s\n", nameW, t.Name, pkgW, pkg, transW, t.Transport, t.Status)
		}
		fmt.Fprintln(w)
	}

	if len(r.Actions) == 0 {
		fmt.Fprintln(w, "No tool actions referenced in this runbook.")
		return
	}

	// Action approval consequences.
	fmt.Fprintln(w, "Action approval consequences:")
	toolW, actionW, classW := 12, 12, 16
	for _, a := range r.Actions {
		if l := len(a.Tool) + 2; l > toolW {
			toolW = l
		}
		if l := len(a.Action) + 2; l > actionW {
			actionW = l
		}
		if l := len(a.Classification) + 2; l > classW {
			classW = l
		}
	}
	hdr := fmt.Sprintf("  %-*s  %-*s  %-*s  %s", toolW, "TOOL", actionW, "ACTION", classW, "CLASSIFICATION", "OUTCOME")
	fmt.Fprintln(w, hdr)
	fmt.Fprintln(w, "  "+strings.Repeat("─", len(hdr)-2))
	for _, a := range r.Actions {
		outcome := a.Outcome
		if a.DenyReason != "" {
			outcome = a.Outcome + " (" + a.DenyReason + ")"
		}
		fmt.Fprintf(w, "  %-*s  %-*s  %-*s  %s\n", toolW, a.Tool, actionW, a.Action, classW, a.Classification, outcome)
	}
}

// splitPlanArgs splits `yawr plan` args into flags, the runbook path, and
// extra positional args. Mirrors splitRunArgs but for plan's flag set.
func splitPlanArgs(args []string) ([]string, string, []string) {
	flagArgs := make([]string, 0, len(args))
	var runbook string
	var extra []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
			// Consume next token as value for flags that take an argument.
			if arg == "--package-map" || arg == "--profile" ||
				arg == "--expand" || arg == "--output" || arg == "--show-profiles" {
				if i+1 < len(args) {
					flagArgs = append(flagArgs, args[i+1])
					i++
				}
			}
			continue
		}
		if runbook == "" {
			runbook = arg
			continue
		}
		extra = append(extra, arg)
	}
	return flagArgs, runbook, extra
}
