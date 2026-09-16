package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/expand"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func scopedPlanWorkspace(t *testing.T) (string, string) {
	t.Helper()
	work := planWorkDir(t)
	root := filepath.Join(work, "root.runbook.yaml")
	writeFile(t, root, `apiVersion: yawr.runbook/v1
id: scoped-plan
name: Scoped plan
flow:
  - step: {id: left, type: include, include: {runbook: left.runbook.yaml}}
  - step: {id: right, type: include, include: {runbook: right.runbook.yaml}}
`)
	for _, side := range []string{"left", "right"} {
		writeFile(t, filepath.Join(work, side+".runbook.yaml"), fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: %s
name: %s
requires: [{package: %s, version: "^1.0.0", path: %s}]
toolRefs: [{name: native-echo, package: %s}]
flow:
  - step: {id: %s_call, type: tool, tool: {name: native-echo, action: run}}
`, side, side, side, side, side, side))
		writeFile(t, filepath.Join(work, side, "yawr-package.yaml"), fmt.Sprintf(`apiVersion: yawr.tool-package/v1
meta: {name: %s, version: "1.0.0"}
exports:
  tools: [{id: native-echo, path: native-echo.tool.yaml}]
`, side))
		tool := strings.Replace(planNativeEchoTool, "command: echo", "command: "+side+"-must-not-execute", 1)
		if side == "right" {
			tool = strings.Replace(tool, "classification: read-only", "classification: mutating", 1)
		}
		writeFile(t, filepath.Join(work, side, "native-echo.tool.yaml"), tool)
	}
	chdirForTest(t, work)
	return work, root
}

func TestPlan_ScopedSiblingAliasesPreserveProvenance(t *testing.T) {
	_, root := scopedPlanWorkspace(t)
	for _, mode := range []string{"eager", "lazy", "auto"} {
		code, output := captureRunPlan(t, []string{root, "--expand", mode, "--output", "json"})
		if code != exitSuccess {
			t.Fatalf("%s: exit=%d output=%s", mode, code, output)
		}
		var result planOutput
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 2 || len(result.Actions) != 2 {
			t.Fatalf("%s: scoped aliases were flattened: %+v", mode, result)
		}
		seen := map[string]bool{}
		for _, binding := range result.Tools {
			if binding.Name != "native-echo" || binding.CanonicalName != "native-echo" ||
				binding.ScopeID == "" || binding.BindingID == "" || binding.DefinitionID == "" ||
				binding.Source == "" || binding.DeclarationSite == "" || binding.SelectionProvenance == "" {
				t.Fatalf("binding lost captured provenance: %+v", binding)
			}
			seen[binding.Package] = true
			if filepath.Base(binding.Source) != strings.Split(binding.Package, "@")[0]+".runbook.yaml" {
				t.Fatalf("binding attached to wrong source: %+v", binding)
			}
		}
		if !seen["left@1.0.0"] || !seen["right@1.0.0"] || result.Tools[0].BindingID == result.Tools[1].BindingID ||
			result.Tools[0].DefinitionID == result.Tools[1].DefinitionID {
			t.Fatalf("same-name bindings share definition or package: %+v", result.Tools)
		}
		for index, action := range result.Actions {
			expected := "read-only"
			if strings.HasSuffix(action.Source, "right.runbook.yaml") {
				expected = "mutating"
			}
			if action.Classification != expected || action.ScopeID != result.Tools[index].ScopeID ||
				action.BindingID != result.Tools[index].BindingID {
				t.Fatalf("action lost its local contract: %+v", action)
			}
		}
	}
	code, output := captureRunPlan(t, []string{root})
	if code != exitSuccess || !strings.Contains(output, "left.runbook.yaml") || !strings.Contains(output, "right.runbook.yaml") ||
		!strings.Contains(output, "binding: sha256:") || !strings.Contains(output, "scope: sha256:") {
		t.Fatalf("text output omitted scoped provenance: exit=%d output=%s", code, output)
	}
}

func TestRouteTestPlanHashScopedMatchesCapturedRunPath(t *testing.T) {
	work, root := scopedPlanWorkspace(t)
	code, output := captureRunPlan(t, []string{root, "--output", "json"})
	if code != exitSuccess {
		t.Fatalf("plan exit=%d output=%s", code, output)
	}

	var displayed planOutput
	if err := json.Unmarshal([]byte(output), &displayed); err != nil {
		t.Fatal(err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	prepared, _, err := prepareScopedCLI(ctx, parser, root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := (&graphdoc.Builder{Loader: prepared.Loader, Recurse: true}).Build(ctx, prepared.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"left", "right"} {
		if err := os.Remove(filepath.Join(work, side+".runbook.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(work, side)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := (&graphdoc.Builder{Loader: prepared.Loader, Recurse: true}).Build(ctx, prepared.Root)
	if err != nil || graph.Hash != after.Hash {
		t.Fatalf("captured graph was rebound to current source: %v", err)
	}
	plan, err := prepared.Plan(ctx, expand.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ToolScopes == nil || len(plan.Tools) != 0 {
		t.Fatal("route hash test did not use scoped execution")
	}
	actual, err := routeTestPlanHash(graph.Hash, plan, prepared.Catalog, prepared.Profile)
	if err != nil || actual != displayed.RouteTestHash {
		t.Fatalf("scoped plan/run route hashes differ: displayed=%s run=%s error=%v", displayed.RouteTestHash, actual, err)
	}
}

func TestPlan_ShowProfilesScopedUnusedChildDeclarations(t *testing.T) {
	work := planWorkDir(t)
	root := filepath.Join(work, "root.runbook.yaml")
	writeFile(t, root, `apiVersion: yawr.runbook/v1
id: root
name: Root
flow:
  - step: {id: child, type: include, include: {runbook: child.runbook.yaml, expand: lazy}}
`)
	writeFile(t, filepath.Join(work, "child.runbook.yaml"), `apiVersion: yawr.runbook/v1
id: child
name: Child
toolRefs: [{name: env-restricted, path: restricted.tool.yaml}]
flow:
  - step: {id: done, type: end, outcome: {category: success, code: ok}}
`)
	writeFile(t, filepath.Join(work, "restricted.tool.yaml"), toolYAMLWithAllowedEnvs("ci"))
	profiles := filepath.Join(work, "profiles")
	good, bad := filepath.Join(profiles, "good.yaml"), filepath.Join(profiles, "bad.yaml")
	writeProfileYAML(t, good, "good", "ci", "unattended")
	writeProfileYAML(t, bad, "bad", "cli-operator", "attended")
	chdirForTest(t, work)

	code, output := captureRunPlan(t, []string{root, "--show-profiles", profiles, "--output", "json"})
	var discovered struct {
		Profiles []struct {
			ID         string `json:"id"`
			Compatible bool   `json:"compatible"`
		} `json:"profiles"`
	}
	if code != exitSuccess {
		t.Fatalf("profile scan exit=%d output=%s", code, output)
	}
	if err := json.Unmarshal([]byte(output), &discovered); err != nil {
		t.Fatal(err)
	}
	if len(discovered.Profiles) != 2 {
		t.Fatalf("missing profile candidates: %+v", discovered)
	}
	for _, profile := range discovered.Profiles {
		if profile.Compatible != (profile.ID == "good") {
			t.Fatalf("unused child declaration was ignored: %+v", profile)
		}
	}
	code, output = captureRunPlan(t, []string{root, "--profile", bad, "--output", "json"})
	var failed planErrorJSON
	if code != exitValidation {
		t.Fatalf("incompatible profile accepted: exit=%d output=%s", code, output)
	}
	if err := json.Unmarshal([]byte(output), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Preflight.Code != "PLAN-010" || len(failed.CompatibleWith) != 1 || failed.CompatibleWith[0] != "good" {
		t.Fatalf("scope-local profile diagnostic lost alternatives: %+v", failed)
	}
}
