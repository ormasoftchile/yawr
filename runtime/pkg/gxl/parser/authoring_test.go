package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/functions"
)

func TestAuthoringCursorLayers(t *testing.T) {
	cases := []struct {
		source, function, item string
		active                 int
	}{
		{"str.contains(str.trim('x,y'), |)", "str.contains", "", 1},
		{"now(|)", "now", "", -1},
		{"regex.match('a', 'str.|')", "", "", -1},
		{"str.trim('str.|')", "", "", -1},
		{"obj.str.contains(|)", "", "", -1},
		{"obj?.list.order(x, 'str.|')", "", "", -1},
		{"list.order(items, 'str.|'", "", "str.contains", -1},
		{"list.order(items, 'str.contains(left.x, |)'", "str.contains", "", 1},
		{"list.order(items, 'regex.match(left.x, \"str.|\")')", "", "", -1},
		{"list.order(items, ('str.|'))", "", "", -1},
		{"list.order(items, 'str.|' + other)", "", "", -1},
		{"str.contains(x[1,2], |)", "str.contains", "", 1},
		{"str.contains('x', 'y', |)", "str.contains", "", -1},
		{"str.contains('x', # comma, ignored\n |)", "str.contains", "", 1},
		{"str.co|ntains('x','y')", "", "str.contains", -1},
		{"obj.str.co|ntains('x','y')", "", "", -1},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			at := strings.Index(c.source, "|")
			src := strings.Replace(c.source, "|", "", 1)
			r := Authoring(context.Background(), src, at)
			if r.Reason != "" {
				t.Fatalf("%+v", r)
			}
			if c.function != "" {
				if r.Function == nil || r.Function.Name != c.function {
					t.Fatalf("signature %+v", r)
				}
				active := r.Active
				if active >= len(r.Function.Parameters()) {
					active = -1
				}
				if active != c.active {
					t.Fatalf("active %d want %d", active, c.active)
				}
			} else if r.Function != nil {
				t.Fatalf("unexpected signature %+v", r)
			}
			if c.item != "" {
				found := false
				for _, i := range r.Items {
					if i.Name == c.item {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing item: %+v", r)
				}
				if strings.Contains(c.source, "co|ntains") && src[r.Start:r.End] != "contains" {
					t.Fatal("mid-token suffix lost")
				}
			} else if c.function == "" && len(r.Items) > 0 {
				t.Fatalf("host layer promoted: %+v", r)
			}
		})
	}
}

func TestAuthoringQualifiedReplacement(t *testing.T) {
	cases := []struct{ source, name, kind, want string }{
		{"st|r.contains('x', 'y')", "str", "namespace", "str.contains('x', 'y')"},
		{"str|.contains('x', 'y')", "str", "namespace", "str.contains('x', 'y')"},
		{"st|Wrong.contains('x', 'y')", "str", "namespace", "str.contains('x', 'y')"},
		{"st|Wrong . co", "str", "namespace", "str . co"},
		{"str|.", "str", "namespace", "str."},
		{"da|Wrong.diffSeconds(x, y)", "date", "namespace", "date.diffSeconds(x, y)"},
		{"li|st.order(rows, 'left.x')", "list", "namespace", "list.order(rows, 'left.x')"},
		{"reg|Wrong.match(x, 'y')", "regex", "namespace", "regex.match(x, 'y')"},
		{"str.co|Wrong('x', 'y')", "str.contains", "function", "str.contains('x', 'y')"},
		{"str.|co", "str.contains", "function", "str.contains"},
		{"list.order(rows, 'st|Wrong.contains(left.x, right.x)')", "str", "namespace", "list.order(rows, 'str.contains(left.x, right.x)')"},
		{`list.order(rows, 's\u0074|Wrong.co')`, "str", "namespace", "list.order(rows, 'str.co')"},
		{"obj.st|r.contains('x', 'y')", "", "", ""},
		{"obj?.st|r.contains('x', 'y')", "", "", ""},
		{"st|r?.contains('x', 'y')", "", "", ""},
		{"str?.co|ntains('x', 'y')", "", "", ""},
		{"str.co|ntains.more", "", "", ""},
		{"str.co|ntains?.more", "", "", ""},
		{"st|r?.['contains']", "", "", ""},
		{"unknown.co|ntains('x', 'y')", "", "", ""},
		{"unknown.|", "", "", ""},
		{"str.trim('st|r.contains')", "", "", ""},
		{"regex.match(x, 'st|r.contains')", "", "", ""},
		{"list.order(rows, 'regex.match(left.x, \"st|r.contains\")')", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.source, func(t *testing.T) {
			src := strings.Replace(c.source, "|", "", 1)
			r := Authoring(context.Background(), src, strings.Index(c.source, "|"))
			if c.name == "" {
				if len(r.Items) != 0 {
					t.Fatalf("invented builtin/member: %+v", r)
				}
				return
			}
			found := false
			for _, item := range r.Items {
				if c.kind == "namespace" && item.Kind != "namespace" {
					t.Fatalf("qualified function at namespace token: %+v", item)
				}
				if item.Name == c.name {
					found = true
					if got := src[:r.Start] + item.Text + src[r.End:]; got != c.want {
						t.Fatalf("edit produced %q, want %q", got, c.want)
					}
				}
			}
			if !found {
				t.Fatalf("missing %s: %+v", c.name, r)
			}
		})
	}
}
func TestAuthoringInventoryAndComparator(t *testing.T) {
	if len(functions.All()) != 18 || functions.Namespace("math") || !functions.Namespace("date") {
		t.Fatal("inventory drift")
	}
	for _, d := range functions.All() {
		parts := strings.SplitN(d.Name, ".", 2)
		if len(parts) == 2 && !knownMethod(parts[0], parts[1]) {
			t.Fatal("parser inventory drift")
		}
	}
	src := "list.order(items, '')"
	r := Authoring(context.Background(), src, strings.Index(src, "''")+1)
	for _, i := range r.Items {
		if i.Name == "now" || i.Name == "list.order" || i.Name == "math" {
			t.Fatal("forbidden comparator inventory")
		}
	}
	if len(r.Items) != 20 {
		t.Fatalf("expected 16 functions + 4 namespaces; got %d: %+v", len(r.Items), r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Authoring(ctx, "str.", 4).Reason != "limit-exceeded" {
		t.Fatal("ignored cancellation")
	}
}
