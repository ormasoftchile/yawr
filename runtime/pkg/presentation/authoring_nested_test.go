package presentation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAuthoringNestedRuntimeFlows(t *testing.T) {
	v, _ := authoringFixture(t)
	wrappers := []struct {
		source, path string
		indent       int
	}{
		{"flow:\n  - iterate:\n      over: '${items}'\n      steps:\n", "/flow/0/iterate/steps/0/step/tool/action", 8},
		{"flow:\n  - parallel:\n      branches:\n        - steps:\n", "/flow/0/parallel/branches/0/steps/0/step/tool/action", 12},
		{"flow:\n  - step:\n      type: branch\n      branches:\n        - condition: true\n          steps:\n", "/flow/0/step/branches/0/steps/0/step/tool/action", 12},
	}
	for _, w := range wrappers {
		child := "- step:\n    id: nested\n    type: tool\n    tool:\n      name: db\n      action: in|CURSOR|\n"
		lines := strings.SplitAfter(child, "\n")
		for i := range lines {
			if lines[i] != "" {
				lines[i] = strings.Repeat(" ", w.indent) + lines[i]
			}
		}
		text := v.Prefix[:strings.Index(v.Prefix, "flow:")] + w.source + strings.Join(lines, "")
		req := v.Base
		req.SchemaVersion = AuthoringRequestVersion
		req.Operation = "complete"
		req.Document = v.Document
		at := strings.Index(text, "|CURSOR|")
		req.Position = utf16Length(text[:at])
		req.Document.Text = strings.Replace(text, "|CURSOR|", "", 1)
		r := ResolveAuthoring(context.Background(), req)
		checkAuthoringReply(t, req, r)
		if r.Status != "resolved" || r.Site == nil || r.Site.YAMLPath != w.path || len(r.Items) != 1 {
			t.Fatalf("%+v", r)
		}
		applyAuthoring(t, req.Document.Text, r.Items[0].Edit)
	}
}

func TestAuthoringProtocolExactContextAndUnicode(t *testing.T) {
	v, _ := authoringFixture(t)
	req := vectorRequest(v, "complete", "        name: d|CURSOR|\n")
	data, _ := json.Marshal(req)
	data = bytes.Replace(data, []byte(`"generation":7`), []byte(`"generation":7,"package_root":""`), 1)
	parsed, err := DecodeAuthoringRequest(bytes.NewReader(data), "complete")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(ResolveAuthoring(context.Background(), parsed))
	if !bytes.Contains(out, []byte(`"package_root":""`)) {
		t.Fatal("optional context presence lost")
	}
	bad := bytes.Replace(data, []byte(`"request_id":"autocomplete-canonical-1"`), []byte(`"request_id":"\ud800"`), 1)
	if _, err := DecodeAuthoringRequest(bytes.NewReader(bad), "complete"); err == nil {
		t.Fatal("unpaired JSON surrogate accepted")
	}
	req.Document.Text = "😀"
	req.Position = 1
	data, _ = json.Marshal(req)
	if _, err := DecodeAuthoringRequest(bytes.NewReader(data), "complete"); err == nil {
		t.Fatal("split surrogate caret accepted")
	}
}

func TestAuthoringAmbiguousMappingAndDepth(t *testing.T) {
	v, _ := authoringFixture(t)
	req := vectorRequest(v, "complete", "        name: db\n        action: inspect\n      when: \"str.\\u00|CURSOR|63ontains('x', 'y')\"\n")
	r := ResolveAuthoring(context.Background(), req)
	if r.Status != "unavailable" || r.Reason != "invalid-source-range" {
		t.Fatal("escape split admitted")
	}
	req = vectorRequest(v, "complete", "        name: db\n        action: inspect\n      when: >-\n        str.\n        |CURSOR|contains('x','y')\n")
	r = ResolveAuthoring(context.Background(), req)
	// A caret in the mapped identifier may be edited, but never a virtual gap.
	if r.Status == "resolved" {
		for _, i := range r.Items {
			applyAuthoring(t, req.Document.Text, i.Edit)
		}
	}
	deep := strings.Repeat("(", 129) + "|CURSOR|" + strings.Repeat(")", 129)
	req = vectorRequest(v, "complete", "        name: db\n        action: inspect\n      when: "+deep+"\n")
	r = ResolveAuthoring(context.Background(), req)
	if r.Status != "unavailable" || r.Reason != "limit-exceeded" {
		t.Fatalf("depth budget ignored %+v", r)
	}
}
