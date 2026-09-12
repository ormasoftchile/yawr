package executor

import (
	"errors"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestWholePublicOutputsStrictProjection(t *testing.T) {
	declarations := map[string]*schema.ArgDef{
		"flag":      {Type: "boolean", From: "response.flag"},
		"zero":      {Type: "integer", From: "response.zero"},
		"nothing":   {Type: "any", From: "response.nothing"},
		"absent":    {Type: "string", Optional: true},
		"stdout":    {Type: "string"},
		"__private": {Type: "string"},
		"terminal":  {Type: "boolean"},
	}
	response := map[string]any{"flag": false, "zero": 0, "nothing": nil}
	raw := map[string]any{"response": response, "stdout": "private-process", "__private": "private-control", "terminal": true, "undeclared": "private-extra"}
	public, err := projectPublicOutputs(declarations, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(public) != 3 || public["flag"] != false || public["zero"] != 0 {
		t.Fatalf("semantic envelope: %#v", public)
	}
	if value, exists := public["nothing"]; !exists || value != nil {
		t.Fatal("null lost")
	}
	response["flag"] = "false"
	if _, err := projectPublicOutputs(declarations, raw); err == nil {
		t.Fatal("whole output coerced string to boolean")
	} else {
		var diagnostic *engine.TypedDiagnostic
		if !errors.As(err, &diagnostic) || diagnostic.Path != "outputs.flag" || diagnostic.Preview != "<redacted>" {
			t.Fatalf("unsafe diagnostic: %v", err)
		}
	}
	if _, err := projectPublicOutputs(nil, raw); err == nil {
		t.Fatal("undeclared outputs admitted")
	}
	response["flag"] = false
	delete(response, "zero")
	if _, err := projectPublicOutputs(declarations, raw); err == nil {
		t.Fatal("required missing value admitted")
	}
}
