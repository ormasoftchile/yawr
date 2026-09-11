package parser

// Only an exact namespace invocation at an expression boundary is a builtin.
// In particular, obj.list.order and obj?.list.order are ordinary properties.
func highlightNamespaceCall(ts []token, open int) (string, string, bool) {
	if open < 3 || open >= len(ts) || ts[open].kind != tokLParen ||
		ts[open-2].kind != tokDot {
		return "", "", false
	}
	ns, method := ts[open-3], ts[open-1]
	if !isNamespaceRoot(ns.lit) || !knownMethod(ns.lit, method.lit) {
		return "", "", false
	}
	if open > 3 {
		prev := ts[open-4]
		switch prev.kind {
		case tokLParen, tokComma, tokEq, tokNeq, tokLTE, tokGTE, tokLT, tokGT,
			tokPlus, tokMinus, tokStar, tokSlash, tokPercent:
		case tokKeyword:
			if prev.lit != "and" && prev.lit != "or" && prev.lit != "not" {
				return "", "", false
			}
		default:
			return "", "", false
		}
	}
	return ns.lit, method.lit, true
}

// Track delimiters, not expression syntax. A role is applied only to a complete,
// single literal argument; computed strings and parenthesized values stay lexical.
func highlightExpressionArguments(ts []token) map[int]argumentRole {
	type frame struct {
		kind      tokenKind
		start     int
		namespace string
		method    string
		arguments []int
	}
	var stack []frame
	out := make(map[int]argumentRole)
	finishArgument := func(f *frame, end int) {
		literal := -1
		if end == f.start+1 && ts[f.start].kind == tokString {
			literal = f.start
		}
		f.arguments = append(f.arguments, literal)
	}
	for i, t := range ts {
		switch t.kind {
		case tokLParen, tokLBracket, tokOptionalBracket:
			if len(stack) == 128 {
				return nil
			}
			ns, method, _ := highlightNamespaceCall(ts, i)
			stack = append(stack, frame{kind: t.kind, start: i + 1, namespace: ns, method: method})
		case tokComma:
			if len(stack) > 0 {
				f := &stack[len(stack)-1]
				finishArgument(f, i)
				f.start = i + 1
			}
		case tokRParen, tokRBracket:
			if len(stack) == 0 {
				continue
			}
			f := &stack[len(stack)-1]
			if (t.kind == tokRParen) != (f.kind == tokLParen) {
				return nil
			}
			finishArgument(f, i)
			for argument, literal := range f.arguments {
				if role := builtinArgumentRole(f.namespace, f.method, argument); literal >= 0 && role != argumentPlain {
					out[literal] = role
				}
			}
			stack = stack[:len(stack)-1]
		}
	}
	return out
}

func highlightLiteral(t token, offsets []int, nested []HighlightToken) []HighlightToken {
	if len(nested) == 0 {
		return nil
	}
	out := make([]HighlightToken, 0, len(nested)+2)
	cursor := t.start.Offset
	for _, n := range nested {
		if n.Start < 0 || n.End <= n.Start || n.End >= len(offsets) {
			return nil
		}
		start, end := offsets[n.Start], offsets[n.End]
		if start < cursor || end <= start || end >= t.end.Offset {
			return nil
		}
		if cursor < start {
			out = append(out, HighlightToken{Start: cursor, End: start, Class: "string"})
		}
		out = append(out, HighlightToken{Start: start, End: end, Class: n.Class})
		cursor = end
	}
	if cursor < t.end.Offset {
		out = append(out, HighlightToken{Start: cursor, End: t.end.Offset, Class: "string"})
	}
	return out
}
