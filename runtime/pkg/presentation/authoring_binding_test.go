package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthoringMetadataRedactionAndRequired(t *testing.T) {
	v, _ := authoringFixture(t)
	v.Base.Overlays[0].Text +=
		"      api_token: {type: secret, required: true, description: SECRET_DESCRIPTION_SENTINEL, default: SECRET_VALUE_SENTINEL}\n" +
			"      null_default: {type: string, required: true, default: null}\n" +
			"      'true': {type: string, required: true}\n" +
			"      'a:b': {type: string, required: true}\n" +
			"      '${evil}\\\\tail': {type: string, required: true}\n"
	req := vectorRequest(v, "complete", "        name: db\n        action: inspect\n        args:\n          |CURSOR|\n")
	r := ResolveAuthoring(context.Background(), req)
	if r.Status != "resolved" {
		t.Fatalf("%+v", r)
	}
	checkAuthoringReply(t, req, r)
	wire, _ := json.Marshal(r)
	if strings.Contains(string(wire), "SENTINEL") {
		t.Fatal("secret metadata leaked")
	}
	for _, i := range r.Items {
		if (i.Name == "limit" || i.Name == "null_default") && i.DefaultInfo != "declared-redacted" {
			t.Fatal("default availability missing")
		}
		applyAuthoring(t, req.Document.Text, i.Edit)
	}
	req = vectorRequest(v, "required-arguments", "        name: db\n        action: inspect\n        args:|CURSOR|\n          text: null # already present\n          limit: 5\n")
	r = ResolveAuthoring(context.Background(), req)
	checkAuthoringReply(t, req, r)
	if r.Status != "resolved" || r.RequiredEdit == nil {
		t.Fatalf("%+v", r)
	}
	text := applyAuthoring(t, req.Document.Text, r.RequiredEdit.Edit)
	if !strings.Contains(text, "text: null # already present") || !strings.Contains(text, "limit: 5") || !strings.Contains(r.RequiredEdit.Edit.NewText, "null_default: null") {
		t.Fatal("required semantics drift")
	}
	if strings.Contains(r.RequiredEdit.Edit.NewText, "text:") || strings.Contains(r.RequiredEdit.Edit.NewText, "limit:") || strings.Contains(text, "SENTINEL") {
		t.Fatal("default/user value copied")
	}
}

func TestAuthoringCatalogScopes(t *testing.T) {
	v, root := authoringFixture(t)
	overlay := func(path, text string) Buffer {
		p := filepath.Join(root, path)
		return Buffer{Path: p, URI: FileURI(p), Version: 1, Text: text}
	}
	t.Run("path-name", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db|CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "name: db\n    path:", "name: d\n    path:", 1)
		req.Position = utf16Length(req.Document.Text[:strings.Index(req.Document.Text, "name: d\n")+len("name: d")])
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) != 1 || r.Items[0].Name != "db" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no-refs", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: |CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "toolRefs:\n  - name: db\n    path: db.tool.yaml\n", "", 1)
		req.Position = utf16Length(req.Document.Text[:len(req.Document.Text)-1])
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) != 0 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("invalid-overlay-no-fallback", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
		if err := os.WriteFile(req.Overlays[0].Path, []byte(req.Overlays[0].Text), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(req.Overlays[0].Path)
		req.Overlays[0].Text = "apiVersion: ["
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" || len(r.Items) > 0 {
			t.Fatal("invalid overlay fell back")
		}
	})
	t.Run("dynamic", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: '${tool}'\n        action: |CURSOR|\n")
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" || r.Reason != "unresolved-dynamic" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("conventional-tools", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "    path: db.tool.yaml\n", "", 1)
		req.Position = utf16Length(req.Document.Text[:len(req.Document.Text)-1])
		req.Overlays[0] = overlay(filepath.Join("tools", "db.tool.yaml"), req.Overlays[0].Text)
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) != 1 {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no-implicit-workspace", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "    path: db.tool.yaml\n", "", 1)
		req.Position = utf16Length(req.Document.Text[:len(req.Document.Text)-1])
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" {
			t.Fatal("implicit workspace discovery")
		}
	})
	t.Run("configured-package-map", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "path: db.tool.yaml", "package: demo", 1)
		req.Position = utf16Length(req.Document.Text[:len(req.Document.Text)-1])
		req.Overlays[0] = overlay(filepath.Join("packages", "demo", "db.tool.yaml"), req.Overlays[0].Text)
		req.Overlays = append(req.Overlays,
			overlay("map.yaml", "apiVersion: yawr.config/v1\nrequires:\n  - {package: demo, version: '^1.0.0', path: packages/demo}\n"),
			overlay(filepath.Join("packages", "demo", "yawr-package.yaml"), "apiVersion: yawr.tool-package/v1\nmeta: {name: demo, version: 1.0.0}\nexports:\n  tools: [{id: db, path: db.tool.yaml}]\n"))
		req.Context.PackageMapPath = req.Overlays[1].Path
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) != 1 {
			t.Fatalf("%+v", r)
		}
		req.Overlays = req.Overlays[:2]
		r = ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" || r.Discovery.Status == "complete" {
			t.Fatal("missing manifest accepted")
		}
	})
	t.Run("builtins-declaration", func(t *testing.T) {
		req := vectorRequest(v, "complete", "        name: db|CURSOR|\n")
		req.Document.Text = strings.Replace(req.Document.Text, "name: db\n    path: db.tool.yaml", "name: ", 1)
		req.Position = utf16Length(req.Document.Text[:strings.Index(req.Document.Text, "  - name: ")+len("  - name: ")])
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || len(r.Items) == 0 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestAuthoringAncestryAndUnavailable(t *testing.T) {
	v, _ := authoringFixture(t)
	for _, body := range []string{
		"        name: db\n        action: in|CURSOR|\n        action: inspect\n",
		"        name: db\n        action: in|CURSOR|\n        <<: *x\n",
		"        name: db\n        args: [\n        action: in|CURSOR|\n",
	} {
		req := vectorRequest(v, "complete", body)
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "unavailable" || len(r.Items) > 0 {
			t.Fatalf("unsafe ancestry admitted: %+v", r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := ResolveAuthoring(ctx, vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n"))
	if r.Status != "stale" || len(r.Items) > 0 {
		t.Fatal("cancellation ignored")
	}
}

func TestAuthoringCatalogLimitDoesNotExcludeHistory(t *testing.T) {
	v, root := authoringFixture(t)
	dir := filepath.Join(root, "tools", ".runbook")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4097; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
	req.Document.Text = strings.Replace(req.Document.Text, "    path: db.tool.yaml\n", "", 1)
	req.Position = utf16Length(req.Document.Text[:len(req.Document.Text)-1])
	r := ResolveAuthoring(context.Background(), req)
	if r.Status != "unavailable" || r.Reason != "limit-exceeded" || r.Discovery.Reason != "limit-exceeded" || len(r.Items) > 0 {
		t.Fatalf("limit hidden: %+v", r)
	}
}

func TestAuthoringSnapshotRevalidation(t *testing.T) {
	v, root := authoringFixture(t)
	path := filepath.Join(root, "metadata.yaml")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &filesystem{ctx: context.Background(), buffers: map[string]Buffer{}, reads: map[string]fileSnapshot{}, deps: map[string]Dependency{}}
	if _, err := f.read(path); err != nil {
		t.Fatal(err)
	}
	if f.stale() {
		t.Fatal("fresh snapshot stale")
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if !f.stale() {
		t.Fatal("changed dependency not detected")
	}
	req := vectorRequest(v, "complete", "        name: db\n        action: |CURSOR|\n")
	r := ResolveAuthoring(context.Background(), req)
	for _, d := range r.Dependencies {
		if d.URI == req.Overlays[0].URI && (d.Version == nil || *d.Version != 9 || d.Digest != Digest([]byte(req.Overlays[0].Text))) {
			t.Fatal("overlay identity lost")
		}
	}
}
