package presentation

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"gopkg.in/yaml.v3"
)

func includeRequest(template string) AuthoringRequest {
	pos := strings.Index(template, "|CURSOR|")
	return AuthoringRequest{SchemaVersion: AuthoringIncludeRequestVersion, Operation: "complete", RequestID: "include",
		Document: Buffer{Text: strings.Replace(template, "|CURSOR|", "", 1)}, Position: utf16Length(template[:pos])}
}

const includePrefix = "apiVersion: yawr.runbook/v1\nid: example\nname: Example 😀\nflow:\n  - step:\n      id: child\n      type: include\n      include:"

func TestAuthoringIncludeEdits(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		for _, v := range []struct {
			name, suffix, item, before, after, kind string
		}{
			{"empty-colon", "|CURSOR| # keep\n", "runbook", "include:", `include: {runbook: ""}`, "include-mapping"},
			{"empty-space", " |CURSOR| # keep\n", "runbook_ref", "include: ", `include: {runbook_ref: "", resolve_from: catalog}`, "include-mapping"},
			{"empty-flow", " {|CURSOR|}\n", "runbook", "{}", `{runbook: ""}`, "include-mapping"},
			{"blank-line", "\n        |CURSOR|\n", "runbook", "        \n", "        runbook: \n", "include-key"},
			{"bare-key", "\n        ru|CURSOR|n\n", "runbook", "        run\n", "        runbook: \n", "include-key"},
			{"quoted-key", "\n        'ru|CURSOR|n': child.yaml # keep\n", "runbook", "'run'", "'runbook'", "include-key"},
			{"double-key", "\n        \"ru|CURSOR|n\": child.yaml\n", "runbook", `"run"`, `"runbook"`, "include-key"},
			{"flow-key", " {'ru|CURSOR|n': child.yaml, with: {value: hi}}\n", "runbook", "'run'", "'runbook'", "include-key"},
			{"explicit-key", "\n        ? ru|CURSOR|n # key\n        : child.yaml # value\n", "runbook", "? run #", "? runbook #", "include-key"},
			{"expand", "\n        runbook: child.yaml\n        expand: l|CURSOR|z # keep\n", "lazy", "expand: lz", "expand: lazy", "include-value"},
			{"expand-quoted", "\n        runbook: child.yaml\n        expand: 'l|CURSOR|z'\n", "lazy", "'lz'", "'lazy'", "include-value"},
			{"enum-empty", "\n        runbook_ref: child\n        resolve_from:|CURSOR| # keep\n", "catalog", "resolve_from:", "resolve_from: catalog", "include-value"},
			{"enum-flow", " {runbook_ref: child, resolve_from: 'ca|CURSOR|t'}\n", "catalog", "'cat'", "'catalog'", "include-value"},
			{"gate-empty", "\n        runbook: child.yaml\n        gate:|CURSOR| # keep\n", "stop_if", "gate:", "gate: {stop_if: []}", "include-mapping"},
			{"gate-key", "\n        runbook: child.yaml\n        gate:\n          st|CURSOR|\n", "stop_if", "          st\n", "          stop_if: \n", "include-key"},
		} {
			t.Run(v.name+strings.ReplaceAll(newline, "\r", "CR"), func(t *testing.T) {
				req := includeRequest(strings.ReplaceAll(includePrefix+v.suffix, "\n", newline))
				reply := ResolveAuthoring(context.Background(), req)
				checkAuthoringReply(t, req, reply)
				if reply.Status != "resolved" || reply.Site == nil || reply.Site.Kind != v.kind {
					t.Fatalf("site: %+v", reply)
				}
				for _, item := range reply.Items {
					if item.Name != v.item {
						continue
					}
					actual := applyAuthoring(t, req.Document.Text, item.Edit)
					want := strings.Replace(req.Document.Text, strings.ReplaceAll(v.before, "\n", newline), strings.ReplaceAll(v.after, "\n", newline), 1)
					if actual != want {
						t.Fatalf("edit mismatch\nactual %q\nwant   %q", actual, want)
					}
					var doc yaml.Node
					if yaml.Unmarshal([]byte(actual), &doc) != nil || !authoringStructure(doc.Content[0], 0) {
						t.Fatal("edited key structure invalid")
					}
					return
				}
				t.Fatalf("missing %q in %+v", v.item, reply)
			})
		}
	}
}

func TestAuthoringIncludeScopeAndArms(t *testing.T) {
	for _, v := range []struct {
		name, source string
		want         []string
	}{
		{"blank-keys", includePrefix + "\n        |CURSOR|\n", []string{"expand", "gate", "on_not_found", "resolve_from", "runbook", "runbook_ref", "when", "with"}},
		{"static-keys", includePrefix + "\n        runbook: child.yaml\n        |CURSOR|\n", []string{"expand", "gate", "when", "with"}},
		{"dynamic-keys", includePrefix + "\n        runbook_ref: child\n        |CURSOR|\n", []string{"gate", "on_not_found", "resolve_from", "when", "with"}},
		{"enum", includePrefix + "\n        runbook_ref: child\n        on_not_found: |CURSOR|\n", []string{"continue", "fail"}},
		{"nested-data", "flow:\n  - step:\n      type: tool\n      tool:\n        name: x\n        args:\n          include:\n            ru|CURSOR|\n", nil},
		{"root-data", "include: |CURSOR|\n", nil},
		{"with-data", includePrefix + "\n        runbook: child.yaml\n        with:\n          include: |CURSOR|\n", nil},
		{"quoted-not-map", includePrefix + " '|CURSOR|'\n", nil},
		{"explicit-no-separator", includePrefix + "\n        ? ru|CURSOR| # keep\n        with: {}\n", nil},
		{"explicit-container-no-separator", strings.TrimSuffix(includePrefix, "include:") + "? include|CURSOR| # keep\n", nil},
		{"explicit-quoted-container-no-separator", strings.TrimSuffix(includePrefix, "include:") + "? 'include'|CURSOR| # keep\n", nil},
		{"gate-no-separator", includePrefix + "\n        runbook: child.yaml\n        ? gate|CURSOR| # keep\n", nil},
		{"static-no-dynamic-enum", includePrefix + "\n        runbook: child.yaml\n        resolve_from: |CURSOR|\n", nil},
		{"both-arms", includePrefix + "\n        runbook: child.yaml\n        runbook_ref: child\n        ex|CURSOR|:\n", nil},
		{"duplicate-ancestry", includePrefix + "|CURSOR|\n      type: tool\n", nil},
		{"addressed-anchor", includePrefix + " &a\n        ru|CURSOR|:\n", nil},
		{"unrelated-anchor", "data: &a {ordinary: value}\nother: *a\n" + includePrefix + "|CURSOR|\n", []string{"runbook", "runbook_ref"}},
		{"comment", includePrefix + " # |CURSOR|\n", nil},
		{"no-type", strings.Replace(includePrefix, "      type: include\n", "", 1) + "|CURSOR|\n", nil},
		{"iterate", "flow:\n  - iterate:\n      steps:\n        - step:\n            type: include\n            include:|CURSOR|\n", []string{"runbook", "runbook_ref"}},
	} {
		t.Run(v.name, func(t *testing.T) {
			reply := ResolveAuthoring(context.Background(), includeRequest(v.source))
			var names []string
			for _, item := range reply.Items {
				names = append(names, item.Name)
			}
			if !reflect.DeepEqual(names, v.want) {
				t.Fatalf("names %v want %v: %+v", names, v.want, reply)
			}
		})
	}
}

func TestAuthoringIncludeSchemaParityAndV1(t *testing.T) {
	spec := includeSchema()
	if spec == nil || len(spec.OneOf) != 2 {
		t.Fatal("missing runtime schema")
	}
	properties := includeProperties(spec, nil, nil)
	typ := reflect.TypeOf(schema.IncludeConfig{})
	for i := 0; i < typ.NumField(); i++ {
		field := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if properties[field] == nil {
			t.Fatalf("schema missing runtime include field %s", field)
		}
	}
	if len(properties) != typ.NumField() {
		t.Fatal("schema/runtime include field drift")
	}
	for _, v := range []struct {
		key    string
		values []string
	}{
		{"resolve_from", []string{schema.ResolveFromCatalog}},
		{"on_not_found", []string{schema.OnNotFoundFail, schema.OnNotFoundContinue}},
	} {
		if !reflect.DeepEqual(properties[v.key].Enum, v.values) {
			t.Fatalf("enum drift %s", v.key)
		}
	}
	req := includeRequest(includePrefix + "|CURSOR|\n")
	req.SchemaVersion = AuthoringRequestVersion
	reply := ResolveAuthoring(context.Background(), req)
	if reply.SchemaVersion != "yawr.authoring-reply/v1" || len(reply.Items) != 0 || reply.Site != nil {
		t.Fatal("v1 changed")
	}
	data, _ := json.Marshal(AuthoringCapabilities())
	if strings.Contains(string(data), "authoring/v2") || strings.Contains(string(data), "capabilities/v2") {
		t.Fatal("historical capabilities changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req.SchemaVersion = AuthoringIncludeRequestVersion
	reply = ResolveAuthoring(ctx, req)
	if reply.Status != "stale" || len(reply.Items) != 0 {
		t.Fatal("cancelled completion escaped")
	}
}
