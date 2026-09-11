package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	parserpkg "github.com/ormasoftchile/yawr/runtime/pkg/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestDebugProfiles_SaveListAndScopeByRootRunbook(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "runbooks", "triage.runbook.yaml")
	writeDebugRunbookFile(t, runbookPath)
	harness := newInteractionHarness(t)
	setProfileTestRunbook(harness, runbookPath, "triage")
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "triage"
	harness.planner.plan.Metadata.PlanHash = "plan-hash-1"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	saveBody := `{
		"runbookPath":` + mustJSON(t, runbookPath) + `,
		"profile":{
			"name":"Mitigated ICM as active",
			"overrides":[{
				"target":{"call_path":[{"step_id":"inspect_primary_icm"}],"step":"get_incident","phase":"after"},
				"set":{"status":"completed","output_patch":{"incident":{"status":"Active"}}}
			}]
		}
	}`
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/debug-profiles/mitigated-icm-as-active", strings.NewReader(saveBody))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("PUT status = %d, want 201; body=%s", response.StatusCode, body)
	}

	profilePath := filepath.Join(workspace, ".yawr", "debug-profiles", "mitigated-icm-as-active.yaml")
	stored, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read stored profile: %v", err)
	}
	text := string(stored)
	closureHash, err := harness.server.debugRunbookClosureHash(context.Background(), runbookPath)
	if err != nil {
		t.Fatalf("closure hash: %v", err)
	}
	planHash, err := debugPlanHash(harness.planner.plan, closureHash)
	if err != nil {
		t.Fatalf("debugPlanHash: %v", err)
	}
	for _, required := range []string{"version: yawr.debug-profile/v1", "ref: runbooks/triage.runbook.yaml", "id: triage", "created_against: " + planHash} {
		if !strings.Contains(text, required) {
			t.Fatalf("stored profile missing %q:\n%s", required, text)
		}
	}

	profiles := listDebugProfiles(t, server.URL, runbookPath)
	if len(profiles) != 1 || profiles[0].ID != "mitigated-icm-as-active" || profiles[0].Stale {
		t.Fatalf("profiles = %#v, want one current profile", profiles)
	}
	if profiles[0].Profile.Root.Ref != "runbooks/triage.runbook.yaml" || profiles[0].Profile.Root.ID != "triage" {
		t.Fatalf("profile root = %#v", profiles[0].Profile.Root)
	}

	other := filepath.Join(workspace, "runbooks", "other.runbook.yaml")
	writeDebugRunbookFile(t, other)
	if got := listDebugProfiles(t, server.URL, other); len(got) != 0 {
		t.Fatalf("profile leaked to another root runbook: %#v", got)
	}
}

func TestDebugProfiles_ListMarksPlanDriftStale(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "triage.runbook.yaml")
	writeDebugRunbookFile(t, runbookPath)
	harness := newInteractionHarness(t)
	setProfileTestRunbook(harness, runbookPath, "triage")
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "triage"
	harness.planner.plan.Metadata.PlanHash = "current-hash"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	dir := filepath.Join(workspace, ".yawr", "debug-profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	profile := `version: yawr.debug-profile/v1
name: Old profile
root:
  ref: triage.runbook.yaml
  id: triage
created_against: old-hash
overrides:
  - target:
      step: get_incident
      phase: after
    set:
      status: completed
`
	if err := os.WriteFile(filepath.Join(dir, "old-profile.yaml"), []byte(profile), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	profiles := listDebugProfiles(t, server.URL, runbookPath)
	if len(profiles) != 1 || !profiles[0].Stale {
		t.Fatalf("profiles = %#v, want one stale profile", profiles)
	}
}

func TestWriteDebugProfile_ReplacementRenameFailurePreservesPreviousProfile(t *testing.T) {
	workspace := t.TempDir()
	harness := newInteractionHarness(t)
	harness.server.cfg.WorkspaceRoot = workspace
	const profileID = "replacement-safety"
	previous := []byte("version: yawr.debug-profile/v1\nname: previous\n")
	replacement := []byte("version: yawr.debug-profile/v1\nname: replacement\n")
	if err := harness.server.writeDebugProfile(profileID, previous); err != nil {
		t.Fatalf("write previous profile: %v", err)
	}

	originalRename := debugProfileRename
	debugProfileRename = func(oldPath, newPath string) error {
		if strings.HasPrefix(filepath.Base(oldPath), ".debug-profile-") && filepath.Base(newPath) == profileID+".yaml" {
			return errors.New("injected replacement rename failure")
		}
		return originalRename(oldPath, newPath)
	}
	t.Cleanup(func() { debugProfileRename = originalRename })

	if err := harness.server.writeDebugProfile(profileID, replacement); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	profilePath := filepath.Join(workspace, ".yawr", "debug-profiles", profileID+".yaml")
	stored, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read previous profile after replacement failure: %v", err)
	}
	if string(stored) != string(previous) {
		t.Fatalf("previous profile changed after replacement failure: got %q, want %q", stored, previous)
	}
}

func TestDebugProfileHashChangesWithExpandedStepSpec(t *testing.T) {
	plan := func(command string) *engine.ExecutionPlan {
		return &engine.ExecutionPlan{
			RunbookPath: "triage.runbook.yaml",
			Steps: []engine.ResolvedStep{{
				ID: "child_step", Kind: "cli", Spec: debugHashStepSpec{Command: command},
			}},
			Metadata: engine.PlanMetadata{RunbookID: "triage", PlanHash: "same-root-hash", PlannedAt: time.Now()},
		}
	}
	left, leftErr := debugPlanHash(plan("first"))
	right, rightErr := debugPlanHash(plan("second"))
	if leftErr != nil || rightErr != nil || left == "" || left == right {
		t.Fatalf("expanded plan hashes = %q/%q, want distinct non-empty hashes", left, right)
	}
}

func TestDebugProfileHashIncludesResolvedToolContracts(t *testing.T) {
	plan := &engine.ExecutionPlan{Tools: map[string]*schema.ToolDef{
		"queryer": {
			APIVersion: "yawr.tool/v1", Name: "queryer",
			Actions: map[string]*schema.ToolAction{
				"query": {Outputs: map[string]*schema.ArgDef{"count": {Type: "integer"}}},
			},
		},
	}}
	before, err := debugPlanHash(plan)
	if err != nil {
		t.Fatalf("before hash: %v", err)
	}
	plan.Tools["queryer"].Actions["query"].Outputs["count"].Type = "string"
	after, err := debugPlanHash(plan)
	if err != nil {
		t.Fatalf("after hash: %v", err)
	}
	if before == after {
		t.Fatalf("resolved tool contract drift did not change hash: %s", before)
	}
}

func TestDebugProfileHashStableAcrossPlanRelocation(t *testing.T) {
	build := func(root string) *engine.ExecutionPlan {
		return &engine.ExecutionPlan{
			RunbookPath: filepath.Join(root, "runbooks", "root.runbook.yaml"),
			Steps: []engine.ResolvedStep{{
				ID: "inspect", Kind: "cli", Origin: filepath.Join(root, "runbooks", "root.runbook.yaml"),
				Spec: debugHashStepSpec{Command: "inspect"},
			}},
			Metadata: engine.PlanMetadata{RunbookID: "root", PlanHash: "semantic-root-hash"},
		}
	}
	left, err := debugPlanHash(build(filepath.Join(t.TempDir(), "left")))
	if err != nil {
		t.Fatalf("left hash: %v", err)
	}
	right, err := debugPlanHash(build(filepath.Join(t.TempDir(), "right")))
	if err != nil {
		t.Fatalf("right hash: %v", err)
	}
	if left != right {
		t.Fatalf("relocated plan hashes differ: %s != %s", left, right)
	}
}

func TestDebugProfileHashRejectsUnserializablePlan(t *testing.T) {
	plan := &engine.ExecutionPlan{
		Steps: []engine.ResolvedStep{{ID: "bad", Kind: "bad", Spec: debugHashBadSpec{Channel: make(chan int)}}},
	}
	if _, err := debugPlanHash(plan); err == nil {
		t.Fatal("unserializable expanded plan fell back to a weaker hash")
	}
}

func TestDebugRunbookClosureHashChangesWhenLazyChildChanges(t *testing.T) {
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.runbook.yaml")
	childPath := filepath.Join(dir, "child.runbook.yaml")
	root := `apiVersion: yawr.runbook/v1
id: root
name: root
flow:
  - step:
      id: child_call
      type: include
      include:
        runbook: child.runbook.yaml
`
	child := func(command string) string {
		return `apiVersion: yawr.runbook/v1
id: child
name: child
flow:
  - step:
      id: command
      type: cli
      command: ` + command + "\n"
	}
	if err := os.WriteFile(rootPath, []byte(root), 0o600); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.WriteFile(childPath, []byte(child("first")), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	server := &Server{parser: parser}
	first, err := server.debugRunbookClosureHash(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("first closure hash: %v", err)
	}
	if err := os.WriteFile(childPath, []byte(child("second")), 0o600); err != nil {
		t.Fatalf("rewrite child: %v", err)
	}
	second, err := server.debugRunbookClosureHash(context.Background(), rootPath)
	if err != nil {
		t.Fatalf("second closure hash: %v", err)
	}
	if first == second {
		t.Fatalf("lazy child change did not alter closure hash: %s", first)
	}
}

func TestDebugRunbookClosureHashStableAcrossRelocation(t *testing.T) {
	writeClosure := func(root string) string {
		runbookPath := filepath.Join(root, "root.runbook.yaml")
		childPath := filepath.Join(root, "child.runbook.yaml")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(runbookPath, []byte("apiVersion: yawr.runbook/v1\nid: root\nname: root\nflow:\n  - step:\n      id: child\n      type: include\n      include: { runbook: child.runbook.yaml }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(childPath, []byte("apiVersion: yawr.runbook/v1\nid: child\nname: child\nkind: composable\nflow:\n  - step: { id: done, type: noop }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return runbookPath
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	server := &Server{parser: parser}
	left, err := server.debugRunbookClosureHash(context.Background(), writeClosure(filepath.Join(t.TempDir(), "left")))
	if err != nil {
		t.Fatalf("left closure hash: %v", err)
	}
	right, err := server.debugRunbookClosureHash(context.Background(), writeClosure(filepath.Join(t.TempDir(), "right")))
	if err != nil {
		t.Fatalf("right closure hash: %v", err)
	}
	if left != right {
		t.Fatalf("relocated closure hashes differ: %s != %s", left, right)
	}
}

func TestDebugProfile_RejectsMissingCreatedAgainst(t *testing.T) {
	profile := &DebugProfile{
		Version: debugProfileVersion,
		Name:    "No hash",
		Root:    DebugProfileRoot{Ref: "triage.runbook.yaml", ID: "triage"},
		Overrides: []DebugProfileOverride{{
			Target: DebugProfileTarget{Step: "get_incident", Phase: engine.DebugPhaseAfter},
			Set:    DebugSet{Status: engine.StepStatusCompleted},
		}},
	}
	if err := validateDebugProfile(profile); err == nil {
		t.Fatal("profile without created_against was accepted")
	}
}

func TestDebugProfiles_RejectsRunbookOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.runbook.yaml")
	writeDebugRunbookFile(t, outside)
	harness := newInteractionHarness(t)
	harness.server.cfg.WorkspaceRoot = workspace
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	response, err := http.Get(server.URL + "/debug-profiles?runbookPath=" + url.QueryEscape(outside))
	if err != nil {
		t.Fatalf("GET profiles: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 200; body=%s", response.StatusCode, body)
	}
	var result struct {
		Profiles []DebugProfileListItem `json:"profiles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || len(result.Profiles) != 0 {
		t.Fatalf("outside-workspace discovery = %#v, err=%v", result, err)
	}
}

func TestDebugProfiles_RejectsSymlinkedRunbookPath(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	outsideRunbook := filepath.Join(outside, "triage.runbook.yaml")
	writeDebugRunbookFile(t, outsideRunbook)
	link := filepath.Join(workspace, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	harness := newInteractionHarness(t)
	harness.server.cfg.WorkspaceRoot = workspace
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	linkedRunbook := filepath.Join(link, "triage.runbook.yaml")
	response, err := http.Get(server.URL + "/debug-profiles?runbookPath=" + url.QueryEscape(linkedRunbook))
	if err != nil {
		t.Fatalf("GET profiles: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 200; body=%s", response.StatusCode, body)
	}
	var result struct {
		Profiles []DebugProfileListItem `json:"profiles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || len(result.Profiles) != 0 {
		t.Fatalf("symlinked discovery = %#v, err=%v", result, err)
	}
}

func TestSameDebugPathUsesPlatformCaseSemantics(t *testing.T) {
	upper := filepath.Join("workspace", "Runbooks", "Triage.runbook.yaml")
	lower := filepath.Join("workspace", "runbooks", "triage.runbook.yaml")
	if !sameDebugPathForOS("windows", upper, lower) {
		t.Fatal("Windows path comparison rejected a case-only spelling difference")
	}
	if sameDebugPathForOS("linux", upper, lower) {
		t.Fatal("case-variant Unix path was treated as the same canonical path")
	}
	if sameDebugPathForOS("darwin", upper, lower) {
		t.Fatal("case-variant Darwin path was treated as the same canonical path")
	}
}

func TestDebugProfiles_RejectsDeclaredSecretVariableOverride(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "triage.runbook.yaml")
	writeDebugRunbookFile(t, runbookPath)
	harness := newInteractionHarness(t)
	setProfileTestRunbook(harness, runbookPath, "triage")
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "triage"
	harness.planner.plan.Inputs = map[string]*schema.Input{"api_secret": {Type: "secret"}}
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	body := `{"runbookPath":` + mustJSON(t, runbookPath) + `,"profile":{"name":"leaky","overrides":[{"target":{"step":"get_incident","phase":"before"},"set":{"vars":{"api_secret":"plaintext-secret"}}}]}}`
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/debug-profiles/leaky", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".yawr", "debug-profiles", "leaky.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret-bearing profile reached disk: %v", err)
	}
}

func TestDebugProfiles_RejectsNestedSecretVariableOverride(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "root.runbook.yaml")
	childPath := filepath.Join(workspace, "child.runbook.yaml")
	root := "apiVersion: yawr.runbook/v1\n" +
		"id: root\n" +
		"name: root\n" +
		"flow:\n" +
		"  - step:\n" +
		"      id: inspect_primary_icm\n" +
		"      type: include\n" +
		"      include:\n" +
		"        runbook: child.runbook.yaml\n" +
		"        with:\n" +
		"          api_secret: \"Bearer ${root_token}\"\n"
	child := `apiVersion: yawr.runbook/v1
id: child
name: child
inputs:
  api_secret:
    type: secret
flow:
  - step:
      id: get_incident
      type: noop
`
	if err := os.WriteFile(runbookPath, []byte(root), 0o600); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.WriteFile(childPath, []byte(child), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	harness := newInteractionHarness(t)
	harness.server.parser = parser
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "root"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	body := `{"runbookPath":` + mustJSON(t, runbookPath) + `,"profile":{"name":"leaky-child","overrides":[{"target":{"call_path":[{"step_id":"inspect_primary_icm"}],"step":"get_incident","phase":"before"},"set":{"vars":{"root_token":"plaintext-secret"}}}]}}`
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/debug-profiles/leaky-child", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), `protected variable "root_token"`) {
		t.Fatalf("body = %q, want protected root_token rejection", raw)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".yawr", "debug-profiles", "leaky-child.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nested secret-bearing profile reached disk: %v", err)
	}
}

func TestDebugProfiles_RejectsVariableOverridesForDynamicIncludes(t *testing.T) {
	workspace := t.TempDir()
	runbookPath := filepath.Join(workspace, "root.runbook.yaml")
	runbook := `apiVersion: yawr.runbook/v1
id: root
name: root
flow:
  - step:
      id: child_call
      type: include
      include:
        runbook_ref: "${child_ref}"
        resolve_from: catalog
`
	if err := os.WriteFile(runbookPath, []byte(runbook), 0o600); err != nil {
		t.Fatalf("write root: %v", err)
	}
	parser, err := internalparser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	harness := newInteractionHarness(t)
	harness.server.parser = parser
	harness.server.cfg.WorkspaceRoot = workspace
	harness.planner.plan.RunbookPath = runbookPath
	harness.planner.plan.Metadata.RunbookID = "root"
	server := newHTTPTestServer(t, harness.server)
	defer server.Close()

	body := `{"runbookPath":` + mustJSON(t, runbookPath) + `,"profile":{"name":"dynamic-vars","overrides":[{"target":{"step":"child_call","phase":"before"},"set":{"vars":{"scenario_value":"plaintext"}}}]}}`
	request, _ := http.NewRequest(http.MethodPut, server.URL+"/debug-profiles/dynamic-vars", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT profile: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 400; body=%s", response.StatusCode, raw)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".yawr", "debug-profiles", "dynamic-vars.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dynamic profile variables reached disk: %v", err)
	}
}

type debugHashStepSpec struct {
	Command string `json:"command"`
}

func (debugHashStepSpec) StepKind() string { return "cli" }

type debugHashBadSpec struct {
	Channel chan int `json:"channel"`
}

func (debugHashBadSpec) StepKind() string { return "bad" }

func listDebugProfiles(t *testing.T, baseURL, runbookPath string) []DebugProfileListItem {
	t.Helper()
	response, err := http.Get(baseURL + "/debug-profiles?runbookPath=" + url.QueryEscape(runbookPath))
	if err != nil {
		t.Fatalf("GET profiles: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET profiles status = %d; body=%s", response.StatusCode, body)
	}
	var result struct {
		Profiles []DebugProfileListItem `json:"profiles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	return result.Profiles
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(encoded)
}

func writeDebugRunbookFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("apiVersion: yawr.runbook/v1\nid: test\nflow: []\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func setProfileTestRunbook(harness *interactionHarness, path, id string) {
	harness.parser.result = &parserpkg.ParsedRunbook{
		Source: path,
		Runbook: &schema.Runbook{
			ID: id, Name: id,
			Flow: []schema.FlowNode{{Step: &schema.Step{ID: "get_incident", Type: schema.StepTypeNoop}}},
		},
	}
}
