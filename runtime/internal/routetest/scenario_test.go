package routetest

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseScenario_StrictReviewedArtifact(t *testing.T) {
	scenario, err := ParseScenario([]byte(strings.ReplaceAll(`apiVersion: yawr.route-test/v1
runbook: runbooks/failover.runbook.yaml
plan_hash: sha256:plan
sensitivity_reviewed: true
target: { step: execute_failover, phase: before, invocation: 1, attempt: 1 }
inputs: { server: db01 }
host_action_responses:
- at: { call_path: [inspect_replication], step: open_external_view, phase: execute, invocation: 1, attempt: 1 }
	capability: external-view.open
	response: { status: completed, result: { status: opened } }
	source: { kind: manual }
	review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
interaction_answers:
- at: { call_path: [inspect_replication, handle_external_view_launch], step: record_findings, phase: execute, invocation: 1, attempt: 1 }
	kind: collector
	values: { primary_health: unavailable }
	source: { kind: manual }
	review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
`, "\t", "  ")))
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
	if scenario.APIVersion != "yawr.route-test/v1" || scenario.Inputs["server"] != "db01" {
		t.Fatalf("scenario = %#v", scenario)
	}
}

func TestParseScenario_RejectsUnknownFieldsAndUnreviewedBindings(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "unknown field",
			yaml: "apiVersion: yawr.route-test/v1\nrunbook: runbook.yaml\nplan_hash: hash\nsensitivity_reviewed: true\ntarget: { step: target, phase: before, invocation: 1, attempt: 1 }\nsurprise: true\n",
			want: "field surprise not found",
		},
		{
			name: "unreviewed response",
			yaml: `apiVersion: yawr.route-test/v1
runbook: runbook.yaml
plan_hash: hash
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
host_action_responses:
- at: { step: open, phase: execute, invocation: 1, attempt: 1 }
	response: { status: completed, result: { status: opened } }
	source: { kind: manual }
	review: { state: draft }
`,
			want: "must be reviewed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseScenario([]byte(strings.ReplaceAll(test.yaml, "\t", "  ")))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseScenario_RejectsInvalidHostActionStatusResultCombinations(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "completed without result", response: `{ status: completed }`, want: "requires result"},
		{name: "failed with result", response: `{ status: failed, result: { state: invalid } }`, want: "must not include result"},
		{name: "timed out with result", response: `{ status: timed-out, result: { state: invalid } }`, want: "must not include result"},
		{name: "not started with result", response: `{ status: execution-not-started, result: { state: invalid } }`, want: "must not include result"},
		{name: "unsupported with result", response: `{ status: unsupported, result: { state: invalid } }`, want: "must not include result"},
		{name: "unknown status", response: `{ status: surprising }`, want: "unknown status"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(`apiVersion: yawr.route-test/v1
runbook: runbook.yaml
plan_hash: hash
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
host_action_responses:
- at: { step: open, phase: execute, invocation: 1, attempt: 1 }
  response: %s
  source: { kind: manual }
  review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
`, test.response)
			_, err := ParseScenario([]byte(document))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseScenario_RejectsStaleResultDigest(t *testing.T) {
	_, err := ParseScenario([]byte(strings.ReplaceAll(`apiVersion: yawr.route-test/v1
runbook: runbook.yaml
plan_hash: hash
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
last_result:
	status: reached
	target_reached: true
	external_dispatches: 0
	ran_at: "2026-08-28T12:05:00Z"
	conditions_digest: sha256:not-real
`, "\t", "  ")))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want digest mismatch", err)
	}
}

func TestParseScenario_AcceptsExtensionConditionsDigest(t *testing.T) {
	_, err := ParseScenario([]byte(strings.ReplaceAll(`apiVersion: yawr.route-test/v1
id: digest-fixture
name: Digest fixture
runbook: runbook.yaml
plan_hash: plan
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
inputs: { region: westus }
interaction_answers:
- at: { step: choice, phase: execute, invocation: 1, attempt: 1 }
  kind: choice
  selected: [primary]
  source: { kind: manual }
  review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
last_result:
  status: reached
  target_reached: true
  external_dispatches: 0
  ran_at: "2026-08-28T12:10:00Z"
	conditions_digest: sha256:883a443caa4a3a692d3cf3123fed4f84ea91cc3a39d2703146ebe108d02eedb6
`, "\t", "  ")))
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
}

func TestParseScenario_AcceptsExtensionDigestWithEmptyOptionalOutput(t *testing.T) {
	_, err := ParseScenario([]byte(strings.ReplaceAll(`apiVersion: yawr.route-test/v1
id: empty-output
name: Empty output
runbook: runbook.yaml
plan_hash: plan
sensitivity_reviewed: true
target: { step: target, phase: before, invocation: 1, attempt: 1 }
step_responses:
- at: { step: command, phase: execute, invocation: 1, attempt: 1 }
	kind: cli
	status: completed
	outcome: success
	source: { kind: manual }
	review: { state: reviewed, reviewed_by: operator, reviewed_at: "2026-08-28T12:05:00Z", sensitivity_reviewed: true }
last_result:
	status: reached
	target_reached: true
	external_dispatches: 0
	ran_at: "2026-08-28T12:10:00Z"
	conditions_digest: sha256:cecd394ab60fc4cc1b521eb3de6da7c82acf87600b11d2a6cb15223ac97110da
`, "\t", "  ")))
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
}

func TestParseScenario_AcceptsExtensionDigestWithEscapedTextAndEmptyCallPath(t *testing.T) {
	_, err := ParseScenario([]byte(strings.ReplaceAll(`apiVersion: yawr.route-test/v1
id: escape-fixture
name: A & B
runbook: runbook.yaml
plan_hash: plan
sensitivity_reviewed: true
target: { call_path: [], step: target, phase: before, invocation: 1, attempt: 1 }
inputs: { region: westus }
last_result:
	status: reached
	target_reached: true
	external_dispatches: 0
	ran_at: "2026-08-28T12:10:00Z"
	conditions_digest: sha256:0f7ca8e3d3a89b6a48edee91d2a1142bdd799c769e2c439b64cd4c74c70f817f
`, "\t", "  ")))
	if err != nil {
		t.Fatalf("ParseScenario: %v", err)
	}
}
