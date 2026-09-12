package parser

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/ormasoftchile/yawr/runtime/pkg/gxl/functions"
)

func registeredMethod(ns, method string) bool {
	_, ok := functions.Lookup(ns + "." + method)
	return ok
}

// CursorResult addresses UTF-8 boundaries in the supplied expression, not YAML.
type CursorResult struct {
	Applicable         bool
	Reason             string
	Start, End         int
	Items              []CursorItem
	Function           *functions.Definition
	Active             int
	CallStart, CallEnd int
}
type CursorItem struct{ Name, Kind, Text string }

func Authoring(ctx context.Context, src string, caret int) CursorResult {
	budget := 65536
	return authoringCursor(ctx, src, caret, false, 0, &budget)
}

func expressionBoundary(ts []token, i int) bool {
	if i < 0 {
		return true
	}
	switch ts[i].kind {
	case tokLParen, tokLBracket, tokOptionalBracket, tokComma, tokEq, tokNeq,
		tokLTE, tokGTE, tokLT, tokGT, tokPlus, tokMinus, tokStar, tokSlash, tokPercent:
		return true
	case tokKeyword:
		return ts[i].lit == "and" || ts[i].lit == "or" || ts[i].lit == "not"
	}
	return false
}

func cursorCall(ts []token, i int, comparator bool) (functions.Definition, int, bool) {
	if ns, method, ok := highlightNamespaceCall(ts, i); ok {
		d, _ := functions.Lookup(ns + "." + method)
		return d, i - 3, !comparator || d.Comparator
	}
	if i > 0 && expressionBoundary(ts, i-2) {
		d, ok := functions.Lookup(ts[i-1].lit)
		return d, i - 1, ok && (!comparator || d.Comparator)
	}
	return functions.Definition{}, 0, false
}

func authoringCursor(ctx context.Context, src string, caret int, comparator bool, depth int, budget *int) CursorResult {
	none := CursorResult{}
	if ctx.Err() != nil || depth > 128 || *budget <= 0 || len(src) > 131072 {
		return CursorResult{Reason: "limit-exceeded"}
	}
	if caret < 0 || caret > len(src) || !utf8.ValidString(src[:caret]) {
		return CursorResult{Reason: "invalid-source-range"}
	}
	l := &lexer{src: src, line: 1, col: 1, ctx: ctx, tokenLimit: *budget + 1}
	err := l.scan()
	*budget -= len(l.tokens) + len(l.comments)
	if *budget < 0 || ctx.Err() != nil {
		return CursorResult{Reason: "limit-exceeded"}
	}
	lastEnd := 0
	if len(l.tokens) > 0 {
		lastEnd = l.tokens[len(l.tokens)-1].end.Offset
	}
	if err != nil && lastEnd <= caret {
		return CursorResult{Reason: "invalid-source-range"}
	}
	for _, c := range l.comments {
		if c.Start <= caret && caret <= c.End {
			return none
		}
	}
	ts := l.tokens
	type frame struct {
		open, start, arg, callStart int
		d                           functions.Definition
		known                       bool
	}
	stack := []frame{}
	result := CursorResult{}
	for i, t := range ts {
		if t.kind == tokString && t.start.Offset < caret && caret < t.end.Offset {
			if len(stack) == 0 {
				return none
			}
			f := stack[len(stack)-1]
			exact := i == f.start && (i+1 == len(ts) || ts[i+1].kind == tokEOF || ts[i+1].kind == tokComma || ts[i+1].kind == tokRParen)
			parts := strings.SplitN(f.d.Name, ".", 2)
			if !exact || !f.known || len(parts) != 2 || builtinArgumentRole(parts[0], parts[1], f.arg) != argumentGXL {
				return none
			}
			offsets := l.stringOffsets[t.start.Offset]
			decoded, ok := t.value.(string)
			if !ok || len(offsets) != len(decoded)+1 {
				return CursorResult{Reason: "invalid-source-range"}
			}
			at := -1
			for j, off := range offsets {
				if off == caret && (j == len(decoded) || utf8.RuneStart(decoded[j])) {
					at = j
					break
				}
			}
			if at < 0 {
				return CursorResult{Reason: "invalid-source-range"}
			}
			n := authoringCursor(ctx, decoded, at, true, depth+1, budget)
			mapRange := func(a, b int) (int, int, bool) {
				if a < 0 || b < a || b >= len(offsets) || offsets[a] < 0 || offsets[b] < 0 {
					return 0, 0, false
				}
				return offsets[a], offsets[b], true
			}
			if n.Applicable {
				var ok bool
				n.Start, n.End, ok = mapRange(n.Start, n.End)
				if !ok {
					return CursorResult{Reason: "invalid-source-range"}
				}
				for j := range n.Items {
					s := strings.ReplaceAll(n.Items[j].Text, "\\", "\\\\")
					q := src[t.start.Offset : t.start.Offset+1]
					n.Items[j].Text = strings.ReplaceAll(s, q, "\\"+q)
				}
			}
			if n.Function != nil {
				var ok bool
				n.CallStart, n.CallEnd, ok = mapRange(n.CallStart, n.CallEnd)
				if !ok {
					return CursorResult{Reason: "invalid-source-range"}
				}
			}
			return n
		}
		if t.start.Offset >= caret {
			break
		}
		switch t.kind {
		case tokLParen, tokLBracket, tokOptionalBracket:
			if len(stack) >= 128 {
				return CursorResult{Reason: "limit-exceeded"}
			}
			d, start, known := cursorCall(ts, i, comparator)
			stack = append(stack, frame{open: i, start: i + 1, d: d, known: known && t.kind == tokLParen, callStart: start})
		case tokComma:
			if len(stack) > 0 {
				f := &stack[len(stack)-1]
				f.arg++
				f.start = i + 1
			}
		case tokRParen, tokRBracket:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	for _, f := range stack {
		if !f.known && ts[f.open].kind == tokLParen && f.open > 0 && (ts[f.open-1].kind == tokIdent || ts[f.open-1].kind == tokKeyword) {
			return none
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		f := stack[i]
		if !f.known {
			continue
		}
		d := f.d
		result.Function = &d
		result.Active = f.arg
		result.CallStart = ts[f.callStart].start.Offset
		result.CallEnd = len(src)
		nesting := 0
		for j := f.open; j < len(ts); j++ {
			switch ts[j].kind {
			case tokLParen, tokLBracket, tokOptionalBracket:
				nesting++
			case tokRParen, tokRBracket:
				nesting--
			}
			if nesting == 0 {
				result.CallEnd = ts[j].end.Offset
				break
			}
		}
		break
	}
	start, end, prev := caret, caret, -1
	for i, t := range ts {
		if (t.kind == tokIdent || t.kind == tokKeyword) && t.start.Offset <= caret && caret <= t.end.Offset {
			start, end, prev = t.start.Offset, t.end.Offset, i-1
			break
		}
		if t.end.Offset <= caret && t.kind != tokEOF {
			prev = i
		}
	}
	ns := ""
	if prev >= 1 && ts[prev].kind == tokDot && functions.Namespace(ts[prev-1].lit) && expressionBoundary(ts, prev-2) {
		ns = ts[prev-1].lit
	} else if !expressionBoundary(ts, prev) {
		return result
	}
	followedByDot := false
	for _, t := range ts {
		if t.start.Offset < end {
			continue
		}
		if t.kind == tokOptionalDot || t.kind == tokOptionalBracket {
			return result
		}
		followedByDot = t.kind == tokDot
		break
	}
	if ns != "" && followedByDot {
		return result
	}
	result.Applicable = true
	result.Start = start
	result.End = end
	prefix := src[start:caret]
	if ns == "" {
		for _, n := range []string{"str", "list", "regex", "date"} {
			if strings.HasPrefix(n, prefix) {
				insert := n + "."
				if followedByDot {
					insert = n
				}
				result.Items = append(result.Items, CursorItem{Name: n, Kind: "namespace", Text: insert})
			}
		}
	}
	// Before an existing member separator, the token is a namespace, not
	// a whole call name. Never replace it with a qualified function.
	if followedByDot {
		return result
	}
	for _, d := range functions.All() {
		if comparator && !d.Comparator {
			continue
		}
		insert := d.Name
		if ns != "" {
			if !strings.HasPrefix(d.Name, ns+".") {
				continue
			}
			insert = strings.TrimPrefix(d.Name, ns+".")
		}
		if strings.HasPrefix(insert, prefix) {
			result.Items = append(result.Items, CursorItem{Name: d.Name, Kind: "function", Text: insert})
		}
	}
	return result
}
