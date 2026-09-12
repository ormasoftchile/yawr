package presentation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type authoringVector struct {
	Base     AuthoringRequest `json:"base_request"`
	Document Buffer           `json:"document_identity"`
	Prefix   string           `json:"prefix"`
	Cases    []struct {
		Name, Operation string
		Source          string `json:"source_template"`
		Target          string `json:"edit_target"`
		Expect          struct {
			Status    string
			Item      AuthoringItem
			Signature AuthoringSignature
		} `json:"expect"`
	} `json:"cases"`
}

func authoringFixture(t *testing.T) (authoringVector, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "authoring-canonical.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != "08500d8713f80a93e218a1fe8a9ce11953eae9eb0a079af0e86caf4301fb5c4e" {
		t.Fatal("canonical fixture drift")
	}
	var v authoringVector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("testdata", ".authoring-"+strings.ReplaceAll(t.Name(), "/", "-")))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	v.Base.Context.ProjectRoot = root
	v.Document.Path = filepath.Join(root, "new.runbook.yaml")
	v.Document.URI = FileURI(v.Document.Path)
	for i := range v.Base.Overlays {
		v.Base.Overlays[i].Path = filepath.Join(root, "db.tool.yaml")
		v.Base.Overlays[i].URI = FileURI(v.Base.Overlays[i].Path)
	}
	return v, root
}
func vectorRequest(v authoringVector, operation, source string) AuthoringRequest {
	req := v.Base
	req.Overlays = append([]Buffer(nil), v.Base.Overlays...)
	req.SchemaVersion = AuthoringRequestVersion
	req.Operation = operation
	req.Document = v.Document
	text := v.Prefix + source
	pos := strings.Index(text, "|CURSOR|")
	req.Document.Text = strings.Replace(text, "|CURSOR|", "", 1)
	req.Position = utf16Length(text[:pos])
	return req
}
func applyAuthoring(t *testing.T, text string, edit AuthoringEdit) string {
	t.Helper()
	a, ok := authoringByteOffset(text, edit.Range.Start)
	if !ok {
		t.Fatal("invalid start")
	}
	b, ok := authoringByteOffset(text, edit.Range.End)
	if !ok || b < a {
		t.Fatal("invalid end")
	}
	result := text[:a] + edit.NewText + text[b:]
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(result), &node); err != nil {
		t.Fatalf("invalid edited YAML: %v", err)
	}
	return result
}
func checkAuthoringReply(t *testing.T, req AuthoringRequest, r AuthoringReply) {
	t.Helper()
	if r.RequestID != req.RequestID || r.Context != req.Context || r.Document.URI != req.Document.URI || r.Document.Version != req.Document.Version || r.Document.Digest != Digest([]byte(req.Document.Text)) {
		t.Fatal("identity drift")
	}
	if r.Items == nil || r.Dependencies == nil {
		t.Fatal("null required array")
	}
	if req.Operation != "complete" && len(r.Items) > 0 || req.Operation != "signature" && r.Signature != nil || req.Operation != "required-arguments" && r.RequiredEdit != nil {
		t.Fatal("cross-operation result")
	}
	if r.Status != "resolved" && r.Reason == "" || r.Status == "resolved" && r.Reason != "" {
		t.Fatal("bad status/reason")
	}
	for _, item := range r.Items {
		if r.Site == nil || item.Edit.Range.Start < r.Site.Range.Start || item.Edit.Range.End > r.Site.Range.End {
			t.Fatalf("edit outside site: %+v / %+v", item, r.Site)
		}
	}
	if r.RequiredEdit != nil {
		if r.Site == nil || r.RequiredEdit.Edit.Range.Start < r.Site.Range.Start || r.RequiredEdit.Edit.Range.End > r.Site.Range.End {
			t.Fatal("required edit outside site")
		}
		for _, p := range r.RequiredEdit.Placeholders {
			a, ok := authoringByteOffset(r.RequiredEdit.Edit.NewText, p.Start)
			b, ok2 := authoringByteOffset(r.RequiredEdit.Edit.NewText, p.End)
			if !ok || !ok2 || r.RequiredEdit.Edit.NewText[a:b] != "null" {
				t.Fatal("invalid placeholder")
			}
		}
	}
}
func TestAuthoringArgumentSeparatorRoundTrip(t *testing.T) {
	v, _ := authoringFixture(t)
	names := []string{"text", "true", "a:b", "a: b", "quote'\"\\${x}", "😀", "line\nbreak"}
	keys := []struct{ name, source, raw string }{
		{"double-spaced", `"te|CURSOR|" : 5 # retained`, `"te"`},
		{"single-spaced", `'te|CURSOR|'  : 5 # retained`, `'te'`},
		{"plain-spaced", `te|CURSOR|   : 5 # retained`, `te`},
		{"double-tab", "\"te|CURSOR|\"\t:\t5 # retained", `"te"`},
		{"escaped-double", `"t\u0065|CURSOR|Wrong" : 5 # retained`, `"t\u0065Wrong"`},
		{"escaped-single", `'t''e|CURSOR|Wrong' : 5 # retained`, `'t''eWrong'`},
		{"immediate-double", `"te|CURSOR|": 5 # retained`, `"te"`},
		{"colon-in-key", `"te: |CURSOR|" : 5 # retained`, `"te: "`},
		{"immediate-plain", `te|CURSOR|: 5 # retained`, `te`},
		{"next-line-value", "\"te|CURSOR|\" : # retained\n            5", `"te"`},
		{"explicit-key", "? \"te|CURSOR|\" # retained\n          : 5", `"te"`},
	}
	for _, key := range keys {
		for _, name := range names {
			for _, crlf := range []bool{false, true} {
				t.Run(key.name+"/"+name+map[bool]string{true: "-crlf"}[crlf], func(t *testing.T) {
					req := vectorRequest(v, "complete", "        name: db\n        action: inspect\n        args:\n          "+key.source+"\n          limit: 9 # sibling\n")
					req.Overlays[0].Text = strings.Replace(req.Overlays[0].Text, "      text:", "      "+yamlName(name)+":", 1)
					if crlf {
						at, _ := authoringByteOffset(req.Document.Text, req.Position)
						req.Position = utf16Length(strings.ReplaceAll(req.Document.Text[:at], "\n", "\r\n"))
						req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", "\r\n")
					}
					r := ResolveAuthoring(context.Background(), req)
					checkAuthoringReply(t, req, r)
					if strings.HasPrefix(key.raw, "'") && strings.Contains(name, "\n") {
						if len(r.Items) != 0 {
							t.Fatal("multiline name cannot be encoded inside single quotes")
						}
						return
					}
					var found *AuthoringItem
					for i := range r.Items {
						if r.Items[i].Name == name {
							found = &r.Items[i]
						}
					}
					if found == nil {
						t.Fatalf("no key item %q: %+v", name, r)
					}
					encoded := yamlName(name)
					if key.raw[0] == '"' {
						encoded = `"` + quoteFragment(name) + `"`
					} else if key.raw[0] == '\'' {
						encoded = "'" + strings.ReplaceAll(name, "'", "''") + "'"
					}
					edited := applyAuthoring(t, req.Document.Text, found.Edit)
					at := strings.Index(req.Document.Text, "\n        args:")
					at += strings.Index(req.Document.Text[at:], key.raw)
					if want := req.Document.Text[:at] + encoded + req.Document.Text[at+len(key.raw):]; edited != want {
						t.Fatalf("separator/value/comment/newline changed:\n%s\nwant:\n%s", edited, want)
					}
					var doc yaml.Node
					if err := yaml.Unmarshal([]byte(edited), &doc); err != nil {
						t.Fatal(err)
					}
					step := mapValue(mapValue(doc.Content[0], "flow").Content[0], "step")
					args := mapValue(mapValue(step, "tool"), "args")
					value := mapValue(args, name)
					if value == nil || value.Tag != "!!int" || value.Value != "5" || len(args.Content) != 4 || mapValue(args, "limit").Value != "9" {
						t.Fatalf("wrong semantic key/value/siblings: %+v", args)
					}
				})
			}
		}
	}
}
func TestAuthoringQuotedKeyWithoutSeparator(t *testing.T) {
	v, _ := authoringFixture(t)
	for _, key := range []string{`? "te|CURSOR|"`, `? 'te|CURSOR|'`} {
		req := vectorRequest(v, "complete", "        name: db\n        action: inspect\n        args:\n          "+key+"\n")
		r := ResolveAuthoring(context.Background(), req)
		checkAuthoringReply(t, req, r)
		if len(r.Items) != 0 {
			t.Fatalf("cannot append a separator inside quoted explicit keys: %+v", r)
		}
	}
}

func TestAuthoringArgumentPairStyles(t *testing.T) {
	v, _ := authoringFixture(t)
	v.Base.Overlays[0].Text += "      '? text': {type: string}\n      'true': {type: string}\n      'a: b': {type: string}\n      'quote''\"\\${x}': {type: string}\n"
	cases := []struct {
		name, body string
		available  bool
	}{
		{"plain", "te|CURSOR|: 5", true},
		{"plain-spaced", "te|CURSOR| \t: 5 # keep", true},
		{"plain-empty", "te|CURSOR|: # keep", true},
		{"bare-repair", "te|CURSOR|", true},
		{"blank-repair", "|CURSOR|", true},
		{"double", "\"te|CURSOR|\" : 5", true},
		{"single", "'te|CURSOR|' : 5", true},
		{"question-plain-name", "?te|CURSOR|: 5", true},
		{"question-double-name", "\"? te|CURSOR|\" : 5", true},
		{"question-single-name", "'? te|CURSOR|' : 5", true},
		{"explicit-plain", "? te|CURSOR|\n          : 5", true},
		{"explicit-comment", "? te|CURSOR| # keep\n          # between\n          : 5", true},
		{"explicit-double", "? \"te|CURSOR|\" # keep\n          : 5", true},
		{"explicit-single", "? 'te|CURSOR|'\n          : # keep\n            5", true},
		{"explicit-empty", "? te|CURSOR|\n          :", true},
		{"explicit-value-map", "? te|CURSOR|\n          : {nested: [5, null, 'kept']}", true},
		{"explicit-key-newline", "?\n            te|CURSOR|\n          : 5", true},
		{"explicit-multiline-plain", "? te|CURSOR|\n            continued\n          : 5", false},
		{"explicit-multiline-double", "? \"te|CURSOR|\n            continued\"\n          : 5", false},
		{"explicit-multiline-single", "? 'te|CURSOR|\n            continued'\n          : 5", false},
		{"explicit-literal-key", "? |-\n            te|CURSOR|\n          : 5", false},
		{"explicit-folded-key", "? >-\n            te|CURSOR|\n          : 5", false},
		{"tagged-key", "!!str te|CURSOR|: 5", false},
		{"explicit-no-separator", "? te|CURSOR|", false},
		{"explicit-comment-no-separator", "? te|CURSOR| # keep", false},
		{"explicit-double-no-separator", "? \"te|CURSOR|\" # keep", false},
		{"explicit-single-no-separator", "? 'te|CURSOR|'", false},
		{"explicit-inline-mapping-key", "? te|CURSOR| : 5", false},
		{"explicit-inline-quoted-mapping-key", "? \"te|CURSOR|\" : 5", false},
		{"explicit-inline-empty-mapping-key", "? te|CURSOR| :", false},
		{"explicit-nonseparating-colon", "? te|CURSOR|\n            :value", false},
		{"explicit-empty-key", "? |CURSOR|\n          : 5", false},
		{"explicit-sequence-key", "? [te|CURSOR|]\n          : 5", false},
		{"explicit-mapping-key", "? {te|CURSOR|: null}\n          : 5", false},
		{"alias-key", "? *te|CURSOR|\n          : 5", false},
		{"anchored-key", "&key te|CURSOR|: 5", false},
		{"merge-key", "<<|CURSOR|: {te: 5}", false},
		{"flow-map", "{te|CURSOR|: 5, limit: 9}", false},
	}
	type nodeIdentity struct {
		Kind       yaml.Kind
		Tag, Value string
		Children   []any
	}
	var identity func(*yaml.Node, *yaml.Node, string) any
	identity = func(n, replaced *yaml.Node, name string) any {
		if n == nil {
			return nil
		}
		out := nodeIdentity{Kind: n.Kind, Tag: n.Tag, Value: n.Value}
		if n.Tag == "!!null" {
			out.Value = ""
		}
		if n == replaced {
			out.Value = name
		}
		for _, child := range n.Content {
			out.Children = append(out.Children, identity(child, replaced, name))
		}
		return out
	}
	for _, tc := range cases {
		for _, crlf := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: "-crlf"}[crlf], func(t *testing.T) {
				body := "        name: db\n        action: inspect\n        args:\n          " + tc.body + "\n          limit: 9 # sibling\n"
				if tc.name == "flow-map" {
					body = "        name: db\n        action: inspect\n        args: " + tc.body + "\n"
				}
				req := vectorRequest(v, "complete", body)
				if crlf {
					at, _ := authoringByteOffset(req.Document.Text, req.Position)
					req.Position = utf16Length(strings.ReplaceAll(req.Document.Text[:at], "\n", "\r\n"))
					req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", "\r\n")
				}
				reply := ResolveAuthoring(context.Background(), req)
				checkAuthoringReply(t, req, reply)
				if !tc.available {
					if len(reply.Items) != 0 || reply.RequiredEdit != nil {
						t.Fatalf("unsupported key offered an edit: %+v", reply)
					}
					if strings.Contains(tc.name, "no-separator") && reply.Status != "unavailable" {
						t.Fatalf("identified unsupported pair must be unavailable: %+v", reply)
					}
					return
				}
				if reply.Status != "resolved" || len(reply.Items) != 5 {
					t.Fatalf("expected every missing argument: %+v", reply)
				}
				caret, _ := authoringByteOffset(req.Document.Text, req.Position)
				before, reason := parseAuthoringSource(req.Document.Text, caret)
				if before == nil {
					t.Fatal(reason)
				}
				target := before.target(context.Background(), "complete")
				if target == nil || target.key == nil || target.key.Kind != yaml.ScalarNode {
					t.Fatal("missing scalar key identity")
				}
				for _, item := range reply.Items {
					if item.Kind != "argument" {
						t.Fatalf("unexpected item: %+v", item)
					}
					edited := applyAuthoring(t, req.Document.Text, item.Edit)
					var doc yaml.Node
					if err := yaml.Unmarshal([]byte(edited), &doc); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(identity(doc.Content[0], nil, ""), identity(before.root, target.key, item.Name)) {
						t.Fatalf("%q changed a key kind, name, value or sibling:\n%s", item.Name, edited)
					}
				}
			})
		}
	}
}

func TestAuthoringRequiredPairStyles(t *testing.T) {
	v, _ := authoringFixture(t)
	for _, body := range []string{
		"limit: 9 # keep",
		"'limit' \t: 9 # keep",
		"? limit\n          : 9 # keep",
		"? limit # key\n          : # value\n            9",
		"? limit # keep",
		"? 'limit' # keep",
		"?\n            limit\n          : 9",
		"? limit\n          : {nested: [5, null, 'kept']}",
		"? 'limit'\n          :",
		"? text # present null",
		"? text\n          : # present empty",
	} {
		for _, suffix := range []string{"\n", "\n          other: 7 # sibling\n"} {
			for _, crlf := range []bool{false, true} {
				t.Run(body+suffix+map[bool]string{true: "-crlf"}[crlf], func(t *testing.T) {
					req := vectorRequest(v, "required-arguments", "        name: db\n        action: inspect\n        args:|CURSOR|\n          "+body+suffix)
					if crlf {
						at, _ := authoringByteOffset(req.Document.Text, req.Position)
						req.Position = utf16Length(strings.ReplaceAll(req.Document.Text[:at], "\n", "\r\n"))
						req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", "\r\n")
					}
					reply := ResolveAuthoring(context.Background(), req)
					checkAuthoringReply(t, req, reply)
					var before yaml.Node
					if err := yaml.Unmarshal([]byte(req.Document.Text), &before); err != nil {
						t.Fatal(err)
					}
					args := mapValue(mapValue(mapValue(mapValue(before.Content[0], "flow").Content[0], "step"), "tool"), "args")
					if mapValue(args, "text") != nil {
						if reply.RequiredEdit != nil {
							t.Fatal("existing explicit null key must count as present")
						}
						return
					}
					if reply.RequiredEdit == nil {
						t.Fatalf("missing required edit: %+v", reply)
					}
					edited := applyAuthoring(t, req.Document.Text, reply.RequiredEdit.Edit)
					var after yaml.Node
					if err := yaml.Unmarshal([]byte(edited), &after); err != nil {
						t.Fatal(err)
					}
					afterArgs := mapValue(mapValue(mapValue(mapValue(after.Content[0], "flow").Content[0], "step"), "tool"), "args")
					if afterArgs.Kind != yaml.MappingNode || len(afterArgs.Content) != len(args.Content)+2 ||
						afterArgs.Content[len(args.Content)].Kind != yaml.ScalarNode ||
						afterArgs.Content[len(args.Content)].Value != "text" ||
						afterArgs.Content[len(args.Content)+1].Tag != "!!null" {
						t.Fatalf("required insertion changed mapping shape: %+v\n%s", afterArgs, edited)
					}
					for i := range args.Content {
						oldBytes, err := yaml.Marshal(args.Content[i])
						if err != nil {
							t.Fatal(err)
						}
						newBytes, err := yaml.Marshal(afterArgs.Content[i])
						if err != nil {
							t.Fatal(err)
						}
						if string(oldBytes) != string(newBytes) {
							t.Fatalf("existing key/value/comment changed: %s / %s", oldBytes, newBytes)
						}
					}
				})
			}
		}
	}
}

func TestAuthoringCanonical(t *testing.T) {
	v, _ := authoringFixture(t)
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			req := vectorRequest(v, c.Operation, c.Source)
			data, _ := json.Marshal(req)
			decoded, err := DecodeAuthoringRequest(bytes.NewReader(data), req.Operation)
			if err != nil {
				t.Fatal(err)
			}
			r := ResolveAuthoring(context.Background(), decoded)
			checkAuthoringReply(t, req, r)
			if r.Status != c.Expect.Status {
				t.Fatalf("%+v", r)
			}
			if c.Expect.Item.Name != "" {
				var found *AuthoringItem
				for i := range r.Items {
					if r.Items[i].Name == c.Expect.Item.Name {
						found = &r.Items[i]
					}
				}
				if found == nil {
					t.Fatalf("missing %s: %+v", c.Expect.Item.Name, r)
				}
				if found.Kind != c.Expect.Item.Kind || found.Edit.NewText != c.Expect.Item.Edit.NewText || c.Expect.Item.Description != "" && found.Description != c.Expect.Item.Description {
					t.Fatalf("item mismatch: %+v", found)
				}
				a, _ := authoringByteOffset(req.Document.Text, found.Edit.Range.Start)
				b, _ := authoringByteOffset(req.Document.Text, found.Edit.Range.End)
				if req.Document.Text[a:b] != c.Target {
					t.Fatalf("wrong target: %q", req.Document.Text[a:b])
				}
				edited := applyAuthoring(t, req.Document.Text, found.Edit)
				if (c.Name == "tool" || c.Name == "action") && strings.Contains(edited, "args:") {
					t.Fatal("implicit argument insertion")
				}
			}
			if c.Name == "signature" && (r.Signature == nil || r.Signature.Name != c.Expect.Signature.Name || r.Signature.Label != c.Expect.Signature.Label || r.Signature.ActiveParameter == nil || *r.Signature.ActiveParameter != 1) {
				t.Fatalf("signature: %+v", r)
			}
			if c.Name == "plain-sql" && (r.Site != nil || len(r.Items) > 0 || r.Signature != nil) {
				t.Fatalf("host string promoted: %+v", r)
			}
			if c.Name == "explicit-required" {
				if r.RequiredEdit == nil || !strings.Contains(r.RequiredEdit.Edit.NewText, "text: null") {
					t.Fatalf("required: %+v", r)
				}
				edited := applyAuthoring(t, req.Document.Text, r.RequiredEdit.Edit)
				if !strings.Contains(edited, "          limit: 5\n") {
					t.Fatal("existing value changed")
				}
			}
		})
	}
}

func TestAuthoringClosedRequest(t *testing.T) {
	v, _ := authoringFixture(t)
	req := vectorRequest(v, "complete", "        name: d|CURSOR|\n")
	data, _ := json.Marshal(req)
	for _, bad := range []string{
		strings.Replace(string(data), `"position":`, `"extra":0,"position":`, 1),
		strings.Replace(string(data), `"position":`, `"position":0,"position":`, 1),
		strings.Replace(string(data), `"generation":7`, `"generation":null`, 1),
		strings.Replace(string(data), `"generation":7`, `"generation":9007199254740992`, 1),
		strings.Replace(string(data), `"text":`, `"extra":0,"text":`, 1),
		strings.Replace(string(data), `"overlays":[`, `"overlays":null,"x":[`, 1),
		string(data) + `{}`, strings.Replace(string(data), `"operation":"complete"`, `"operation":"signature"`, 1),
	} {
		if _, err := DecodeAuthoringRequest(strings.NewReader(bad), "complete"); err == nil {
			t.Fatal("accepted malformed request")
		}
	}
}

func TestAuthoringSourceAndRequired(t *testing.T) {
	v, _ := authoringFixture(t)
	cases := []struct{ name, op, body, status, want string }{
		{"blank-name", "complete", "        name: |CURSOR|\n        action: inspect\n", "resolved", "db"},
		{"blank-action", "complete", "        name: db\n        action: |CURSOR|\n", "resolved", "inspect"},
		{"blank-no-space", "complete", "        name: db\n        action:|CURSOR|\n", "resolved", "inspect"},
		{"blank-comment", "complete", "        name: db\n        action: |CURSOR|# preserved\n", "resolved", "inspect"},
		{"boolean-fragment", "complete", "        name: true|CURSOR|\n", "resolved", "db"},
		{"empty-argument-line", "complete", "        name: db\n        action: inspect\n        args:\n          |CURSOR|\n", "resolved", "text"},
		{"mid-token", "complete", "        name: d|CURSOR|x\n        action: inspect\n", "resolved", "db"},
		{"quoted-action", "complete", "        name: db\n        action: 'in|CURSOR|'\n", "resolved", "inspect"},
		{"duplicate-tool", "complete", "        name: db\n        name: d|CURSOR|\n", "unavailable", ""},
		{"nested-value", "complete", "        name: db\n        action: inspect\n        args:\n          text:\n            te|CURSOR|: value\n", "resolved", ""},
		{"null-present", "required-arguments", "        name: db\n        action: inspect\n        args:|CURSOR|\n          text: null\n", "resolved", ""},
		{"missing-args", "required-arguments", "        name: db\n        action: inspect|CURSOR|\n", "resolved", "required"},
		{"flow-args", "required-arguments", "        name: db\n        action: inspect\n        args:|CURSOR| {limit: 5}\n", "unavailable", ""},
		{"later-broken", "complete", "        name: db\n        action: in|CURSOR|\n  - step: [\n", "resolved", "inspect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := vectorRequest(v, c.op, c.body)
			r := ResolveAuthoring(context.Background(), req)
			checkAuthoringReply(t, req, r)
			if r.Status != c.status {
				t.Fatalf("%+v", r)
			}
			if c.want == "required" {
				if r.RequiredEdit == nil {
					t.Fatalf("%+v", r)
				}
				applyAuthoring(t, req.Document.Text, r.RequiredEdit.Edit)
			} else if c.want != "" {
				found := false
				for _, item := range r.Items {
					if item.Name == c.want {
						found = true
						if c.name != "later-broken" {
							applyAuthoring(t, req.Document.Text, item.Edit)
						}
					}
				}
				if !found {
					t.Fatalf("missing %s: %+v", c.want, r)
				}
			} else if r.Status == "resolved" && (len(r.Items) > 0 || r.RequiredEdit != nil) {
				t.Fatalf("unexpected result %+v", r)
			}
		})
	}
}
