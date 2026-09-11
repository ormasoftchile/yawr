package presentation

import (
	"context"
	"strings"
	"testing"
)

func TestAuthoringExpressionSourceRoundTrip(t *testing.T) {
	v, _ := authoringFixture(t)
	cases := []struct{ name, body, want, raw string }{
		{"plain", "      when: str.co|CURSOR|ntains('a', 'b')\n", "str.contains", "contains"},
		{"blank-expression", "      when:|CURSOR|\n", "str.contains", ""},
		{"emoji", "      title: '😀'\n      when: str.co|CURSOR|ntains('😀', 'b')\n", "str.contains", "contains"},
		{"single", "      when: 'str.co|CURSOR|ntains(''a'', ''b'')'\n", "str.contains", "contains"},
		{"double", "      when: \"str.co|CURSOR|ntains('a', 'b')\"\n", "str.contains", "contains"},
		{"unicode", "      when: \"str.\\u0063o|CURSOR|ntains('a', 'b')\"\n", "str.contains", "\\u0063ontains"},
		{"literal", "      when: |-\n        str.co|CURSOR|ntains('a', 'b')\n", "str.contains", "contains"},
		{"folded", "      when: >-\n        str.co|CURSOR|ntains('a',\n          'b')\n", "str.contains", "contains"},
		{"nested", "      when: \"list.order(items, 'str.co|CURSOR|ntains(left.x, right.x)'\"\n", "str.contains", "contains"},
		{"nested-escape", "      when: \"list.order(items, 'str.\\\\u0063o|CURSOR|ntains(left.x, right.x)'\"\n", "str.contains", "\\\\u0063ontains"},
		{"gis", "        args:\n          text: 'SQL ${str.co|CURSOR|ntains(''a'', ''b'')}'\n", "str.contains", "contains"},
		{"ordinary", "        args:\n          text: 'str.co|CURSOR|ntains'\n", "", ""},
		{"regex", "      when: regex.match('x', 'str.co|CURSOR|ntains')\n", "", ""},
		{"object", "      when: obj.str.co|CURSOR|ntains('x')\n", "", ""},
	}
	for _, c := range cases {
		for _, crlf := range []bool{false, true} {
			t.Run(c.name+map[bool]string{true: "-crlf"}[crlf], func(t *testing.T) {
				req := vectorRequest(v, "complete", "        name: db\n        action: inspect\n"+c.body)
				if crlf {
					prefix, _ := authoringByteOffset(req.Document.Text, req.Position)
					req.Position = utf16Length(strings.ReplaceAll(req.Document.Text[:prefix], "\n", "\r\n"))
					req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", "\r\n")
				}
				r := ResolveAuthoring(context.Background(), req)
				checkAuthoringReply(t, req, r)
				if r.Status != "resolved" {
					t.Fatalf("%+v", r)
				}
				if c.want == "" {
					if len(r.Items) > 0 || r.Signature != nil {
						t.Fatal("host string promoted")
					}
					return
				}
				var found *AuthoringItem
				for i := range r.Items {
					if r.Items[i].Name == c.want {
						found = &r.Items[i]
					}
				}
				if found == nil {
					t.Fatalf("no item %+v", r)
				}
				a, _ := authoringByteOffset(req.Document.Text, found.Edit.Range.Start)
				b, _ := authoringByteOffset(req.Document.Text, found.Edit.Range.End)
				if req.Document.Text[a:b] != c.raw {
					t.Fatalf("target %q != %q", req.Document.Text[a:b], c.raw)
				}
				applyAuthoring(t, req.Document.Text, found.Edit)
			})
		}
	}
}

func TestAuthoringNamespaceSourceRoundTrip(t *testing.T) {
	v, _ := authoringFixture(t)
	cases := []struct{ name, body, raw string }{
		{"before-dot", "      when: str|CURSOR|.contains('x', 'y') # keep\n", "str"},
		{"mid-namespace", "      when: st|CURSOR|Wrong.contains('x', 'y') # keep\n", "stWrong"},
		{"half-method", "      when: st|CURSOR|Wrong . co\n", "stWrong"},
		{"single", "      when: 'st|CURSOR|Wrong.contains(''x'', ''y'')' # keep\n", "stWrong"},
		{"double", "      when: \"s\\u0074|CURSOR|Wrong.contains('x', 'y')\" # keep\n", "s\\u0074Wrong"},
		{"literal", "      when: |-\n        st|CURSOR|Wrong.contains('x', 'y')\n", "stWrong"},
		{"folded", "      when: >-\n        st|CURSOR|Wrong.contains('x',\n          'y')\n", "stWrong"},
		{"comparator", "      when: \"list.order(rows, 'st|CURSOR|Wrong.contains(left.x, right.x)')\"\n", "stWrong"},
		{"comparator-escape", "      when: \"list.order(rows, 's\\\\u0074|CURSOR|Wrong.co')\"\n", "s\\\\u0074Wrong"},
		{"gis", "        args:\n          text: 'SQL ${st|CURSOR|Wrong.contains(''x'', ''y'')}'\n", "stWrong"},
	}
	for _, c := range cases {
		for _, crlf := range []bool{false, true} {
			t.Run(c.name+map[bool]string{true: "-crlf"}[crlf], func(t *testing.T) {
				body := "        name: db\n        action: inspect\n" + c.body + "      title: '😀'\n"
				req := vectorRequest(v, "complete", body)
				if crlf {
					at, _ := authoringByteOffset(req.Document.Text, req.Position)
					req.Position = utf16Length(strings.ReplaceAll(req.Document.Text[:at], "\n", "\r\n"))
					req.Document.Text = strings.ReplaceAll(req.Document.Text, "\n", "\r\n")
				}
				r := ResolveAuthoring(context.Background(), req)
				checkAuthoringReply(t, req, r)
				if r.Status != "resolved" || len(r.Items) != 1 || r.Items[0].Name != "str" || r.Items[0].Kind != "namespace" {
					t.Fatalf("namespace-only context: %+v", r)
				}
				got := applyAuthoring(t, req.Document.Text, r.Items[0].Edit)
				want := strings.Replace(req.Document.Text, c.raw, "str", 1)
				if got != want {
					t.Fatalf("wrong expression or changed surrounding bytes:\n%s\nwant:\n%s", got, want)
				}
			})
		}
	}
}
func TestAuthoringSignatureNullAndLabels(t *testing.T) {
	v, _ := authoringFixture(t)
	for _, expression := range []string{"now(|CURSOR|)", "str.trim('x', |CURSOR|)", "str.contains(str.trim('x,y'), |CURSOR|)", "list.order(items, 'str.contains(left.x, |CURSOR|)'"} {
		req := vectorRequest(v, "signature", "        name: db\n        action: inspect\n      when: "+expression+"\n")
		r := ResolveAuthoring(context.Background(), req)
		if r.Status != "resolved" || r.Signature == nil {
			t.Fatalf("%+v", r)
		}
		if strings.HasPrefix(expression, "now") || strings.HasPrefix(expression, "str.trim") {
			if r.Signature.ActiveParameter != nil {
				t.Fatal("clamped active parameter")
			}
		}
		for _, p := range r.Signature.Parameters {
			a, _ := authoringByteOffset(r.Signature.Label, p.LabelRange.Start)
			b, _ := authoringByteOffset(r.Signature.Label, p.LabelRange.End)
			if !strings.HasPrefix(r.Signature.Label[a:b], p.Name+":") {
				t.Fatal("label span drift")
			}
		}
	}
}
