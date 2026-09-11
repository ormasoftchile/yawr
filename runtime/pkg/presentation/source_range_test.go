package presentation

import (
	"strings"
	"testing"
	"unicode/utf16"

	"gopkg.in/yaml.v3"
)

func TestBlockScalarTokenOwnership(t *testing.T) {
	for _, header := range []string{"|-", "|", "|+", ">-", ">", ">+", "|2-", "|-2"} {
		for _, eol := range []string{"\n", "\r\n"} {
			for _, tail := range []string{"", "\n", "\n\n", "  \n", "    \n"} {
				for _, leading := range []string{"", "\n"} {
					text := "key: " + header + "\n" + leading + "  SELECT 🚀\n  FROM t\n" + tail
					text = strings.ReplaceAll(text, "\n", eol)
					var document yaml.Node
					if err := yaml.Unmarshal([]byte(text), &document); err != nil {
						t.Fatal(err)
					}

					got, ok := scalarRange(text, document.Content[0].Content[1], 1)
					if !ok {
						t.Fatalf("no range for %q", text)
					}
					expected := text
					if !strings.Contains(header, "+") && tail != "    \n" {
						expected = strings.TrimSuffix(expected, strings.ReplaceAll(tail, "\n", eol))
					}
					if got.Start != 5 || got.End != len(utf16.Encode([]rune(expected))) {
						t.Fatalf("%q: %+v want end %d", text, got, len(utf16.Encode([]rune(expected))))
					}
				}
			}
		}
	}
}

func TestNestedExplicitBlockIndentationAndEOF(t *testing.T) {
	for _, header := range []string{"|2-", "|-2", ">2", "|2+", "|"} {
		for _, eol := range []string{"\n", "\r\n"} {
			for _, tail := range []string{"", "\n", "\n\n"} {
				text := strings.ReplaceAll("root:\n    key: "+header+"\n      SELECT 🚀\n      FROM t"+tail, "\n", eol)
				var doc yaml.Node
				if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
					t.Fatal(err)
				}
				value := mapValue(mapValue(doc.Content[0], "root"), "key")
				got, ok := scalarRange(text, value, 5)
				expected := text
				if !strings.Contains(header, "+") && tail == "\n\n" {
					expected = strings.TrimSuffix(expected, eol)
				}
				if !ok || got.End != len(utf16.Encode([]rune(expected))) {
					t.Fatalf("nested range %q: %+v valid=%v", text, got, ok)
				}
			}
		}
	}
}
