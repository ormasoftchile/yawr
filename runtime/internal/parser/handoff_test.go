package parser_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestParser_HandoffDecodesStaticTargetAndAllowlistedContext(t *testing.T) {
	parserImpl, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parsed, err := parserImpl.ParseBytes(context.Background(), []byte(handoffRunbook(`
      runbook: GEODR0004.runbook.yaml
      reason:
        code: active-update-slo
        summary: Continue with active Update SLO investigation
      with:
        environment: "${environment}"
        logical_server: "${logical_server}"
      facts:
        active_workflow: "${active_workflow}"`)))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	step := parsed.Runbook.Flow[0].Step
	if step.Type != schema.StepTypeHandoff || step.HandoffSpec == nil {
		t.Fatalf("handoff was not decoded: %#v", step)
	}
	handoff := step.HandoffSpec.Handoff
	if handoff.Runbook != "GEODR0004.runbook.yaml" || handoff.Reason.Code != "active-update-slo" ||
		handoff.With["logical_server"] != "${logical_server}" ||
		handoff.Facts["active_workflow"] != "${active_workflow}" {
		t.Fatalf("decoded handoff = %#v", handoff)
	}
}

func TestParser_HandoffRejectsDynamicTraversalAndSensitiveBindings(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"dynamic target": {body: `
      runbook: "${next_runbook}.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"traversal target": {body: `
      runbook: ../private.runbook.yaml
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"drive-relative target": {body: `
      runbook: "C:target.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"alternate-stream target": {body: `
      runbook: "file:stream.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"reserved-device target": {body: `
      runbook: CON.runbook.yaml
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"surrounding whitespace target": {body: `
      runbook: "target.runbook.yaml "
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"leading whitespace target": {body: `
      runbook: " bad.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"invalid question target": {body: `
      runbook: "bad?.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"invalid wildcard target": {body: `
      runbook: "bad*.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"invalid pipe target": {body: `
      runbook: "bad|.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"invalid angle target": {body: `
      runbook: "bad<name>.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"superscript com target": {body: `
      runbook: "COM\u00b9.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"superscript lpt target": {body: `
      runbook: "LPT\u00b9.runbook.yaml"
      reason: { code: next, summary: Continue }`, want: "static relative"},
		"missing reason summary": {body: `
      runbook: next.runbook.yaml
      reason: { code: next }`, want: "summary"},
		"sensitive input": {body: `
      runbook: next.runbook.yaml
      reason: { code: next, summary: Continue }
      with: { access_token: "${access_token}" }`, want: "non-sensitive"},
		"sensitive fact": {body: `
      runbook: next.runbook.yaml
      reason: { code: next, summary: Continue }
      facts: { password: "${password}" }`, want: "non-sensitive"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			parserImpl, err := parser.New(platform.NewFakePlatform())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = parserImpl.ParseBytes(context.Background(), []byte(handoffRunbook(test.body)))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseBytes error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestParser_HandoffRejectsAliasOfDeclaredSecretInput(t *testing.T) {
	parserImpl, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source := `apiVersion: yawr.runbook/v1
id: handoff-test
name: Handoff test
kind: mitigation
inputs:
  opaque:
    type: secret
flow:
  - step:
      id: continue_in_target
      type: handoff
      handoff:
        runbook: next.runbook.yaml
        reason: { code: next, summary: Continue }
        with: { server: "${opaque}" }
`
	_, err = parserImpl.ParseBytes(context.Background(), []byte(source))
	if err == nil || !strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("ParseBytes error = %v, want value-free secret source refusal", err)
	}
}

func TestParser_HandoffRejectsVarsScopeEscapeToSecret(t *testing.T) {
	parserImpl, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source := `apiVersion: yawr.runbook/v1
id: handoff-test
name: Handoff test
kind: mitigation
inputs:
  opaque:
    type: secret
flow:
  - step:
      id: continue_in_target
      type: handoff
      handoff:
        runbook: next.runbook.yaml
        reason: { code: next, summary: Continue }
        with: { server: "${opaque}" }
`
	_, err = parserImpl.ParseBytes(context.Background(), []byte(source))
	if err == nil || !strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("ParseBytes error = %v, want value-free vars-scope refusal", err)
	}
}

func TestParser_HandoffRejectsParallelDescendant(t *testing.T) {
	parserImpl, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: handoff-test
name: Handoff test
kind: mitigation
flow:
	- parallel:
			id: gather
			branches:
				- label: first
					steps:
						- step:
								id: continue_in_target
								type: handoff
								handoff:
									runbook: next.runbook.yaml
									reason: { code: next, summary: Continue }
				- label: second
					steps:
						- step:
								id: inspect
								type: noop
`, "\t", "  ")
	_, err = parserImpl.ParseBytes(context.Background(), []byte(source))
	if err == nil || !strings.Contains(err.Error(), "handoff") || !strings.Contains(err.Error(), "parallel") {
		t.Fatalf("ParseBytes error = %v, want handoff-in-parallel refusal", err)
	}
}

func TestParser_HandoffRejectsConcurrentIterateDescendant(t *testing.T) {
	parserImpl, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source := strings.ReplaceAll(`apiVersion: yawr.runbook/v1
id: handoff-test
name: Handoff test
kind: mitigation
flow:
	- iterate:
			id: each_target
			over: "${targets}"
			as: target
			concurrency: 2
			steps:
				- step:
						id: continue_in_target
						type: handoff
						handoff:
							runbook: next.runbook.yaml
							reason: { code: next, summary: Continue }
`, "\t", "  ")
	_, err = parserImpl.ParseBytes(context.Background(), []byte(source))
	if err == nil || !strings.Contains(err.Error(), "handoff") || !strings.Contains(err.Error(), "parallel") {
		t.Fatalf("ParseBytes error = %v, want concurrent-iterate handoff refusal", err)
	}
}

func handoffRunbook(body string) string {
	body = "  " + strings.ReplaceAll(strings.TrimPrefix(body, "\n"), "\n", "\n  ")
	return `apiVersion: yawr.runbook/v1
id: handoff-test
name: Handoff test
kind: mitigation
flow:
  - step:
      id: continue_in_target
      type: handoff
      handoff:
` + body + `
`
}
