package gis

import "testing"

func TestParseSegmentsAndEscapes(t *testing.T) {
	tmpl, err := Parse(`hello \${name} ${user.name}\n`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(tmpl.Segments), 3; got != want {
		t.Fatalf("segments = %d, want %d", got, want)
	}
	lit, ok := tmpl.Segments[0].(*Literal)
	if !ok || lit.Value != "hello ${name} " {
		t.Fatalf("first literal = %#v", tmpl.Segments[0])
	}
	if _, ok := tmpl.Segments[1].(*Expr); !ok {
		t.Fatalf("second segment = %T, want *Expr", tmpl.Segments[1])
	}
	lit, ok = tmpl.Segments[2].(*Literal)
	if !ok || lit.Value != "\n" {
		t.Fatalf("last literal = %#v", tmpl.Segments[2])
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		code string
	}{
		{name: "unterminated", in: "${user.name", code: "GIS-PARSE-001"},
		{name: "empty", in: "${}", code: "GIS-PARSE-004"},
		{name: "invalid gxl", in: "${?.root}", code: "GIS-PARSE-003"},
		{name: "dollar doubling", in: "$${name}", code: "GIS-PARSE-003"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.in)
			if err == nil {
				t.Fatal("Parse() error = nil")
			}
			if got := errorCode(err); got != tt.code {
				t.Fatalf("code = %s, want %s (%v)", got, tt.code, err)
			}
		})
	}
}

func TestParseExpressionWithBraceInString(t *testing.T) {
	tmpl, err := Parse(`${str.trim("}")}`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := len(tmpl.Segments), 1; got != want {
		t.Fatalf("segments = %d, want %d", got, want)
	}
}
