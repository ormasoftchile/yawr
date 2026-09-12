package presentation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAuthoringFullSourceAdmission(t *testing.T) {
	v, root := authoringFixture(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "authoring-full-shape.runbook.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if boundedSource(doc.Content[0], 0) || !authoringStructure(doc.Content[0], 0) {
		t.Fatal("fixture must isolate unrelated anchors/aliases, not duplicate mappings or malformed YAML")
	}
	if len(mapValue(doc.Content[0], "flow").Content) != 15 {
		t.Fatal("full caller flow shape drift")
	}
	overlay := func(p, text string) Buffer {
		p = filepath.Join(root, p)
		return Buffer{URI: FileURI(p), Path: p, Version: 1, Text: text}
	}
	v.Prefix = ""
	v.Base.Overlays = []Buffer{
		overlay(filepath.Join("packages", "query", "sample.tool.yaml"),
			"apiVersion: yawr.tool/v1\nmeta: {name: sample-query, version: 1.0.0}\nactions:\n  - name: query\n    args:\n      environment: {type: string, required: true}\n      timeout: {type: string}\n      query: {type: string, required: true}\n"),
		overlay("map.yaml", "apiVersion: yawr.config/v1\nrequires:\n  - {package: example.query-tools, version: '^1.0.0', path: packages/query}\n"),
		overlay(filepath.Join("packages", "query", "yawr-package.yaml"),
			"apiVersion: yawr.tool-package/v1\nmeta: {name: example.query-tools, version: 1.0.0}\nexports:\n  tools: [{id: sample-query, path: sample.tool.yaml}]\n"),
	}
	v.Base.Context.PackageMapPath = v.Base.Overlays[1].Path
	for _, tc := range []struct {
		name, old, replacement, kind, item string
	}{
		{"complete-action", "action: query", "action: query|CURSOR|", "action", "query"},
		{"unsaved-partial-action", "action: query", "action: q|CURSOR|", "action", "query"},
		{"argument-key", "timeout: \"120\"", "ti|CURSOR|: \"120\"", "argument", "timeout"},
		{"bare-argument-key", "timeout: \"120\"", "ti|CURSOR|", "argument", "timeout"},
		{"date-subject", "date.compare(start_time", "date.com|CURSOR|pare(start_time", "expression", "date.compare"},
		{"date-comparator", "date.diffSeconds(left", "date.dif|CURSOR|fSeconds(left", "expression", "date.diffSeconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := vectorRequest(v, "complete", strings.Replace(text, tc.old, tc.replacement, 1))
			if strings.HasPrefix(tc.name, "unsaved") {
				req.Document.URI = strings.Replace(req.Document.URI, "file:", "untitled:", 1)
			}
			r := ResolveAuthoring(context.Background(), req)
			checkAuthoringReply(t, req, r)
			if r.Status != "resolved" || r.Site == nil || r.Site.Kind != tc.kind {
				t.Fatalf("%+v", r)
			}
			found := false
			for _, item := range r.Items {
				if item.Name == tc.item {
					found = true
					applyAuthoring(t, req.Document.Text, item.Edit)
				}
			}
			if !found {
				t.Fatalf("missing %s: %+v", tc.item, r)
			}
		})
	}
	t.Run("anchored-earlier-source-with-later-incomplete-step", func(t *testing.T) {
		template := strings.Replace(text, "action: query", "action: q|CURSOR|", 1)
		template += "  - step: [\n"
		req := vectorRequest(v, "complete", template)
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) != 1 || r.Items[0].Name != "query" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("missing-manifest", func(t *testing.T) {
		req := vectorRequest(v, "complete", strings.Replace(text, "action: query", "action: q|CURSOR|", 1))
		req.Overlays = req.Overlays[:2]
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" || r.Reason != "incomplete-identity" || r.Discovery.Status != "limited" || len(r.Items) != 0 {
			t.Fatalf("%+v", r)
		}
		missing := false
		for _, d := range r.Dependencies {
			if d.URI == v.Base.Overlays[2].URI && d.Missing {
				missing = true
			}
		}
		if !missing {
			t.Fatal("missing manifest dependency not reported")
		}
	})
}

func TestAuthoringAddressedAncestry(t *testing.T) {
	v, _ := authoringFixture(t)
	base := v.Prefix + "        name: db\n        action: in|CURSOR|\n"
	v.Prefix = ""
	for _, tc := range []struct {
		name, source string
		safe         bool
	}{
		{"unrelated-scalar-anchor", "metadata: {pattern: &utc '^[0-9]{4}$', reuse: *utc}\n" + base, true},
		{"unrelated-container-anchor", "metadata: {sample: &sample {text: ordinary}, reuse: *sample}\n" + base, true},
		{"duplicate-tool", strings.Replace(base, "        name: db", "        name: db\n        name: db", 1), false},
		{"duplicate-flow-prefix", "flow: []\n" + base + "  - step: [\n", false},
		{"broken-prefix-string", "metadata: 'unterminated\n" + base + "  - step: [\n", false},
		{"duplicate-later-flow", base + "flow: [\n", false},
		{"duplicate-after-prefix-cut", base + "  - step: [\nflow: []\n", false},
		{"identity-after-prefix-cut", base + "  - step: [\ntoolRefs: []\n", false},
		{"anchored-root", "&root\n" + base, false},
		{"anchored-flow", strings.Replace(base, "flow:", "flow: &steps", 1), false},
		{"anchored-step", strings.Replace(base, "- step:", "- step: &step", 1), false},
		{"anchored-tool", strings.Replace(base, "      tool:", "      tool: &tool", 1), false},
		{"anchored-name", strings.Replace(base, "name: db\n        action", "name: &name db\n        action", 1), false},
		{"anchored-type", strings.Replace(base, "type: tool", "type: &kind tool", 1), false},
		{"aliased-type", "metadata: &kind tool\n" + strings.Replace(base, "type: tool", "type: *kind", 1), false},
		{"aliased-name", "metadata: &name db\n" + strings.Replace(base, "name: db\n        action", "name: *name\n        action", 1), false},
		{"merge-tool", "metadata: &identity {name: db}\n" + strings.Replace(base, "        name: db", "        <<: *identity", 1), false},
		{"anchored-reference", strings.Replace(base, "  - name: db", "  - &ref\n    name: db", 1), false},
		{"dynamic-name", strings.Replace(base, "name: db\n        action", "name: '${selected}'\n        action", 1), false},
		{"later-incomplete-step", base + "  - step: [\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := vectorRequest(v, "complete", tc.source)
			r := ResolveAuthoring(context.Background(), req)
			checkAuthoringReply(t, req, r)
			if tc.safe {
				if r.Status != "resolved" || len(r.Items) != 1 || r.Items[0].Name != "inspect" {
					t.Fatalf("%+v", r)
				}
			} else if len(r.Items) != 0 || r.Signature != nil || r.RequiredEdit != nil {
				t.Fatalf("unsafe source admitted: %+v", r)
			}
		})
	}
}
