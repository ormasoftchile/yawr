package parser_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestParser_HostActionDecodesStructuredRequest(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parsed, err := p.ParseBytes(context.Background(), []byte(hostActionRunbook(`
      capability: product.open-resource
      request:
        resource: incident-42
        options:
          focus: true`)))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	step := parsed.Runbook.Flow[0].Step
	if step.Type != schema.StepTypeHostAction || step.HostActionSpec == nil {
		t.Fatalf("host action was not decoded: %#v", step)
	}
	request := step.HostActionSpec.HostAction
	options, _ := request.Request["options"].(map[string]any)
	if request.Capability != "product.open-resource" ||
		request.Request["resource"] != "incident-42" ||
		options["focus"] != true {
		t.Fatalf("decoded request: %#v", request)
	}
}

func TestParser_HostActionAcceptsOpaqueCapabilityRequest(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parsed, err := p.ParseBytes(context.Background(), []byte(hostActionRunbook(`
      capability: product.open-resource
      request:
        resource: "${resource.name}"
        options:
          focus: true
          retries: 2
          labels: [primary, "${resource.region}"]`)))
	if err != nil {
		t.Fatalf("opaque host action rejected: %v", err)
	}
	request := parsed.Runbook.Flow[0].Step.HostActionSpec.HostAction
	if request.Capability != "product.open-resource" {
		t.Fatalf("capability = %q", request.Capability)
	}
}

func TestParser_HostActionRejectsOversizedStaticRequestValue(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	source := hostActionRunbook(`
      capability: product.open-resource
      request:
        resource: "` + strings.Repeat("x", hostactionMaxStaticString+1) + `"`)
	_, err = p.ParseBytes(context.Background(), []byte(source))
	if err == nil || !strings.Contains(err.Error(), "schema/structural") {
		t.Fatalf("oversized static host-action request was accepted: %v", err)
	}
}

const hostactionMaxStaticString = 4096

func TestParser_HostActionRejectsFieldsOutsideTheEnvelope(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, body := range map[string]string{
		"flat-alias": `
      capability: product.open-resource
      resource: incident-42`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseBytes(context.Background(), []byte(hostActionRunbook(body)))
			if err == nil {
				t.Fatal("expected rejected host action")
			}
			if !strings.Contains(err.Error(), "schema/structural") {
				t.Fatalf("expected structural schema error, got %v", err)
			}
		})
	}

	t.Run("top-level-command", func(t *testing.T) {
		source := hostActionRunbook(`
      capability: product.open-resource
      request:
        resource: incident-42`)
		source = strings.Replace(source, "      host_action:", "      command: workbench.action.files.openFile\n      host_action:", 1)
		_, err := p.ParseBytes(context.Background(), []byte(source))
		if err == nil || !strings.Contains(err.Error(), "schema/structural") {
			t.Fatalf("top-level host command was accepted: %v", err)
		}
	})
}

func TestParser_HostActionTestEchoAccepted(t *testing.T) {
	p, err := parser.New(platform.NewFakePlatform())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parsed, err := p.ParseBytes(context.Background(), []byte(hostActionRunbook(`
      capability: test.echo
      request:
        echo: hello-e2e`)))
	if err != nil {
		t.Fatalf("test.echo runbook rejected: %v", err)
	}
	step := parsed.Runbook.Flow[0].Step
	if step.Type != schema.StepTypeHostAction || step.HostActionSpec == nil {
		t.Fatalf("host action was not decoded: %#v", step)
	}
	if step.HostActionSpec.HostAction.Capability != "test.echo" {
		t.Fatalf("capability: got %q, want %q", step.HostActionSpec.HostAction.Capability, "test.echo")
	}
	if step.HostActionSpec.HostAction.Request["echo"] != "hello-e2e" {
		t.Fatalf("echo: got %q, want %q", step.HostActionSpec.HostAction.Request["echo"], "hello-e2e")
	}
}

func hostActionRunbook(action string) string {
	action = "  " + strings.ReplaceAll(strings.TrimPrefix(action, "\n"), "\n", "\n  ")
	return `apiVersion: yawr.runbook/v1
id: host-action-test
name: Host action test
flow:
  - step:
      id: open_xts_view
      type: host_action
      host_action:
` + action + `
`
}
