package presentation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"
)

const canonicalContractDigest = "sha256:5264150ec2208082a21448bca2eeb1f64549bb84bc9e1033fd912beca140ded8"

func canonicalContractBytes(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\n"), []byte("\r\n"))
}

func contract(t *testing.T) (Request, map[string]any, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "interfaces.md"))
	if err != nil {
		t.Fatal(err)
	}
	vector := regexp.MustCompile("(?s)```json\\r?\\n(.*?)\\r?\\n```").FindSubmatch(data)
	if len(vector) != 2 || Digest(canonicalContractBytes(vector[1])) != canonicalContractDigest {
		t.Fatal("canonical vector digest changed")
	}
	var v struct {
		Request Request        `json:"request"`
		Expect  map[string]any `json:"expect"`
		Invalid string         `json:"invalid_runbook_text"`
	}
	if err := json.Unmarshal(vector[1], &v); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	v.Request.Context.ProjectRoot = root
	for _, b := range []*Buffer{&v.Request.Document, &v.Request.Overlays[0]} {
		name := b.Path[strings.LastIndex(b.Path, `\`)+1:]
		b.Path = filepath.Join(root, name)
		b.URI = FileURI(b.Path)
	}
	v.Expect["context"] = map[string]any{"project_root": root, "generation": float64(7)}
	v.Expect["document"] = map[string]any{"uri": v.Request.Document.URI, "version": float64(3)}
	return v.Request, v.Expect, v.Invalid
}

func TestCanonicalContractDigestIgnoresCheckoutLineEndings(t *testing.T) {
	lf := []byte("{\n  \"contract\": true\n}")
	crlf := []byte("{\r\n  \"contract\": true\r\n}")
	if !bytes.Equal(canonicalContractBytes(lf), canonicalContractBytes(crlf)) {
		t.Fatal("canonical contract bytes depend on checkout line endings")
	}
	if bytes.Equal(canonicalContractBytes(lf), canonicalContractBytes([]byte("{\n  \"contract\": false\n}"))) {
		t.Fatal("canonicalization erased a meaningful contract change")
	}
}

func partial(t *testing.T, want, got any, path string) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("%s: not object: %v", path, got)
		}
		for k, v := range w {
			partial(t, v, g[k], path+"/"+k)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			t.Fatalf("%s: array mismatch: %#v", path, got)
		}
		for i := range w {
			partial(t, w[i], g[i], path)
		}
	default:
		if !reflect.DeepEqual(w, got) {
			t.Fatalf("%s: got %#v want %#v", path, got, want)
		}
	}
}
func TestPresentationCanonicalProductionReply(t *testing.T) {
	req, expect, invalid := contract(t)
	wire, _ := json.Marshal(req)
	decoded, err := DecodeRequest(strings.NewReader(string(wire)))
	if err != nil {
		t.Fatal(err)
	}
	reply := Resolve(decoded)
	data, _ := json.Marshal(reply)
	var got any
	json.Unmarshal(data, &got)
	partial(t, expect, got, "")
	if strings.Contains(string(data), "SELECT 1") || strings.Contains(string(data), "never-execute") {
		t.Fatal("source values leaked")
	}
	if len(reply.Dependencies) < 3 {
		t.Fatalf("missing dependencies: %#v", reply.Dependencies)
	}
	req.Document.Text = invalid
	reply = Resolve(req)
	if reply.Status != "unavailable" || reply.Reason != "incomplete-source" || len(reply.Regions) != 0 {
		t.Fatalf("invalid source recovered identity: %#v", reply)
	}
}
func TestPresentationDirtyToolAndUnsupported(t *testing.T) {
	req, _, _ := contract(t)
	req.Overlays[0].Text = strings.ReplaceAll(req.Overlays[0].Text, "language: sql", "language: future")
	reply := Resolve(req)
	if reply.Bindings[0].Actions[0].Arguments[0].Reason != "unsupported-language" {
		t.Fatalf("%#v", reply)
	}
	req.Overlays[0].Text = strings.ReplaceAll(req.Overlays[0].Text, "kind: code", "kind: secret")
	reply = Resolve(req)
	if reply.Bindings[0].Reason != "invalid-descriptor" || len(reply.Regions) != 0 {
		t.Fatalf("%#v", reply)
	}
}
func TestPresentationRanges(t *testing.T) {
	for _, token := range []string{"'SELECT 😀'", "\"SELECT \\\"😀\\\"\"", "|-\n            SELECT 😀\n            FROM t\n", ">-\n            SELECT 😀\n            FROM t\n"} {
		for _, eol := range []string{"\n", "\r\n"} {
			t.Run(token+eol, func(t *testing.T) {
				req, _, _ := contract(t)
				req.Document.Text = strings.Replace(req.Document.Text, "'SELECT 1'", token, 1)
				req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", eol)
				reply := Resolve(req)
				if len(reply.Regions) != 1 {
					t.Fatalf("no proven scalar: %#v", reply)
				}
				r := reply.Regions[0].Range
				got := string(utf16.Decode(utf16.Encode([]rune(req.Document.Text))[r.Start:r.End]))
				if !strings.HasPrefix(got, strings.ReplaceAll(token, "\n", eol)) {
					t.Fatalf("range %q want %q", got, token)
				}
			})
		}
	}
}
func TestPresentationPartialFlowRetainsBinding(t *testing.T) {
	req, _, _ := contract(t)
	req.Document.Text += "  - step: [broken\n"
	reply := Resolve(req)
	if len(reply.Bindings) != 1 || reply.Bindings[0].Status != "resolved" || len(reply.Regions) != 1 {
		t.Fatalf("%#v", reply)
	}
}
func TestPresentationEnvelopeLimits(t *testing.T) {
	req, _, _ := contract(t)
	for _, change := range []func(*Request){func(r *Request) { r.Context.Generation = -1 }, func(r *Request) { r.Document.URI = "file:///wrong" }, func(r *Request) {
		duplicate := r.Overlays[0]
		duplicate.Version++
		r.Overlays = append(r.Overlays, duplicate)
	}, func(r *Request) { r.Document.Version = 9007199254740992 }} {
		copy := req
		change(&copy)
		data, _ := json.Marshal(copy)
		if _, err := DecodeRequest(strings.NewReader(string(data))); err == nil {
			t.Fatal("invalid request accepted")
		}

	}
}

func TestPresentationPackageBareAmbiguityAndConfigOverlays(t *testing.T) {
	req, _, _ := contract(t)
	root := req.Context.ProjectRoot
	add := func(path, text string) {
		path = filepath.Join(root, path)
		req.Overlays = append(req.Overlays, Buffer{Path: path, URI: FileURI(path), Version: 1, Text: text})
	}
	req.Overlays[0].Path = filepath.Join(root, "packages", "demo", "db.tool.yaml")
	req.Overlays[0].URI = FileURI(req.Overlays[0].Path)
	add(filepath.Join(".yawr", "config.yaml"), "apiVersion: yawr.config/v1\nrequires:\n  - {package: demo, version: '^1.0.0', path: packages/demo}\n")
	add(filepath.Join("packages", "demo", "yawr-package.yaml"), "apiVersion: yawr.tool-package/v1\nmeta: {name: demo, version: 1.0.0}\nexports:\n  tools: [{id: db, path: db.tool.yaml}]\n")
	req.Document.Text = strings.Replace(req.Document.Text, "path: db.tool.yaml", "package: demo", 1)
	reply := Resolve(req)
	if len(reply.Bindings) != 1 || reply.Bindings[0].Status != "resolved" {
		t.Fatalf("package overlays not bound: %#v", reply)
	}
	req.Document.Text = strings.Replace(req.Document.Text, "    package: demo\n", "", 1)
	reply = Resolve(req)
	if reply.Bindings[0].Status != "resolved" {
		t.Fatalf("bare package not bound: %#v", reply)
	}
	add(filepath.Join("tools", "db.tool.yaml"), req.Overlays[0].Text)
	reply = Resolve(req)
	if reply.Bindings[0].Status != "ambiguous" || reply.Bindings[0].Reason != "ambiguous-binding" {
		t.Fatalf("cross-tier ambiguity lost: %#v", reply)
	}
	req.Overlays[1].Text = "apiVersion: malformed\n"
	reply = Resolve(req)
	if reply.Reason != "incomplete-identity" || len(reply.Regions) != 0 {
		t.Fatalf("dirty invalid config fell back to disk: %#v", reply)
	}
}

func TestPresentationLexicalIncludedDocument(t *testing.T) {
	req, _, _ := contract(t)
	entry := req.Document
	req.Context.EntrypointPath = entry.Path
	req.Overlays = append(req.Overlays, entry)
	req.Document.Path = filepath.Join(req.Context.ProjectRoot, "child.runbook.yaml")
	req.Document.URI = FileURI(req.Document.Path)
	req.Document.Text = strings.Replace(req.Document.Text, "toolRefs:\n  - name: db\n    path: db.tool.yaml\n", "", 1)
	reply := Resolve(req)
	if len(reply.Bindings) != 0 || len(reply.Regions) != 0 {
		t.Fatal("entrypoint toolRefs leaked into lexical child")
	}
}

func TestPresentationSnapshotRecheck(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "db.tool.yaml")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	f := &filesystem{buffers: map[string]Buffer{}, reads: map[string]fileSnapshot{}, deps: map[string]Dependency{}}
	if _, err := f.read(path); err != nil {
		t.Fatal(err)
	}
	if f.stale() {
		t.Fatal("unchanged snapshot stale")
	}
	if err := os.WriteFile(path, []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if !f.stale() {
		t.Fatal("changed dependency published")
	}
}
