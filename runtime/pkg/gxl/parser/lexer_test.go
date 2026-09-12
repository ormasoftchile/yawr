package parser

import "testing"

func TestLexTokens(t *testing.T) {
	toks, err := lex("a == 1 and str.contains(b, 'x') # tail")
	if err != nil {
		t.Fatal(err)
	}
	want := []tokenKind{tokIdent, tokEq, tokNumber, tokKeyword, tokKeyword, tokDot, tokIdent, tokLParen, tokIdent, tokComma, tokString, tokRParen, tokEOF}
	if len(toks) != len(want) {
		t.Fatalf("len(tokens) = %d, want %d: %#v", len(toks), len(want), toks)
	}
	for i := range want {
		if toks[i].kind != want[i] {
			t.Fatalf("token[%d] = %v, want %v", i, toks[i].kind, want[i])
		}
	}
}

func TestLexStringEscapes(t *testing.T) {
	toks, err := lex(`"line\n\t\u2603"`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := toks[0].value, "line\n\t\u2603"; got != want {
		t.Fatalf("string value = %#v, want %#v", got, want)
	}
}

func TestLexInvalidEscape(t *testing.T) {
	if _, err := lex(`"bad\xFF"`); err == nil {
		t.Fatal("lex succeeded, want invalid escape")
	}
}
