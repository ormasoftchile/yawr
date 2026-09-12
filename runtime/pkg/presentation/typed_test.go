package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTypedAuthoringDiscriminatorsAndBindingScope(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{"apiVersion: yawr.runbook/v1\nid: t\nname: T\nflow:\n  - step:\n      id: r\n      type: re|CURSOR|\n", "results"},
		{"apiVersion: yawr.runbook/v1\nid: t\nname: T\nbindings:\n  - name: earlier\n    type: ar|CURSOR|\n    value: []\nflow: []\n", "array"},
		{"apiVersion: yawr.runbook/v1\nid: t\nname: T\nbindings:\n  - name: earlier\n    type: string\n    value: hello\n  - name: later\n    type: string\n    value: '${ear|CURSOR|}'\nflow: []\n", "earlier"},
	} {
		req := includeRequest(test.source)
		req.SchemaVersion = AuthoringRequestVersion
		reply := ResolveAuthoring(context.Background(), req)
		if reply.SchemaVersion != "authoring-reply/v3" || reply.Status != "resolved" {
			t.Fatalf("%+v", reply)
		}
		found := false
		for _, item := range reply.Items {
			if item.Name == test.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %+v site %+v", test.want, reply.Items, reply.Site)
		}
		for _, item := range reply.Items {
			if item.Name == "later" {
				t.Fatal("forward binding suggested")
			}
		}
	}
}

func TestExpressionRegionsIncludeTypedTrees(t *testing.T) {
	text := "apiVersion: yawr.runbook/v1\nid: t\nname: T\nbindings:\n  - name: a\n    type: string\n    value: '${input}'\noutputs:\n  result:\n    type: object\n    value_tree: {nested: '${a}'}\nflow:\n  - step:\n      id: a\n      type: assign\n      assign: [{name: a, value: '${input}'}]\n  - step: {id: r, type: results}\n"
	reply := ResolveExpressions(context.Background(), expressionRequest(t, text))
	if reply.Status != "resolved" || reply.SchemaVersion != ExpressionSchemaVersion || len(reply.Regions) != 3 {
		t.Fatalf("missing typed tree sites: %+v", reply)
	}
}

func TestCapabilitiesExposeOnlyCurrentAuthoringEnvelope(t *testing.T) {
	body, _ := json.Marshal(AuthoringCapabilities())
	if !bytes.Contains(body, []byte("authoring-capabilities/v3")) ||
		bytes.Contains(body, []byte("authoring-capabilities/v1")) ||
		bytes.Contains(body, []byte("authoring-capabilities/v2")) {
		t.Fatal("authoring capabilities are not current-only")
	}
	body, _ = json.Marshal(TypedResultsCapabilities())
	for _, cap := range []string{"presentation-capabilities/v3", "execution-plan/v3", "yawr.run-results-chunks/v1", "yawr.run-get-results/v1"} {
		if !strings.Contains(string(body), cap) {
			t.Fatal("missing capability", cap)
		}
	}
}
