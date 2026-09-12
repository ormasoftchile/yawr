package tool

import (
	"testing"

	runtimetool "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestAuthoringProjectionBindsDeclaredIdentity(t *testing.T) {
	data := []byte("apiVersion: yawr.tool/v1\nmeta: {name: actual}\nactions:\n  - name: inspect\n    args:\n      token: {type: secret, required: true, description: DO_NOT_PUBLISH, default: DO_NOT_PUBLISH}\n")
	bound := runtimetool.ToolDef{Name: "file-local-name", Source: "tool://actual", SourcePath: "offline.tool.yaml", Actions: map[string]*runtimetool.ToolAction{
		"inspect": {Args: map[string]*runtimetool.ArgDef{"token": {Type: "secret"}}},
	}}
	got, err := AuthoringProjection(data, bound)
	if err != nil {
		t.Fatal(err)
	}
	arg := got.Actions["inspect"].Arguments["token"]
	if arg.Description != "" || !arg.Required || !arg.HasDefault {
		t.Fatal("redaction/presence drift")
	}
	bound.Source = "tool://different"
	if _, err := AuthoringProjection(data, bound); err == nil {
		t.Fatal("unbound metadata identity accepted")
	}
}
