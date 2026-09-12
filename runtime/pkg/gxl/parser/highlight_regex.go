package parser

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HighlightRegex scans Go/RE2 lexical syntax without matching or evaluating.
// Unsupported constructs retain a string suffix; this is not a diagnostic.
func HighlightRegex(ctx context.Context, src string) []HighlightToken {
	budget := 65536
	return highlightRegex(ctx, src, &budget)
}

func highlightRegex(ctx context.Context, src string, budget *int) []HighlightToken {
	if len(src) > 32768*3 || !utf8.ValidString(src) {
		return nil
	}
	var out []HighlightToken
	add := func(start, end int, class string) bool {
		if start == end {
			return true
		}
		if *budget <= 0 || ctx.Err() != nil {
			return false
		}
		*budget--
		out = append(out, HighlightToken{start, end, class})
		return true
	}
	inClass, classFirst, quoted := false, false, false
	for i := 0; i < len(src); {
		if ctx.Err() != nil || *budget <= 0 {
			return nil
		}
		start := i
		if quoted {
			if strings.HasPrefix(src[i:], `\E`) {
				add(i, i+2, "string")
				i += 2
				quoted = false
			} else {
				for i < len(src) && !strings.HasPrefix(src[i:], `\E`) {
					_, size := utf8.DecodeRuneInString(src[i:])
					i += size
				}
				add(start, i, "string")
			}
			continue
		}
		if src[i] == '\\' {
			end, class, ok := regexEscape(src, i, inClass)
			if !ok {
				add(i, len(src), "string")
				break
			}
			add(i, end, class)
			quoted = !inClass && src[i:end] == `\Q`
			i = end
			classFirst = false
			continue
		}
		if inClass {
			if classFirst && src[i] == '^' {
				add(i, i+1, "operator")
				i++
				// A closing bracket after ^ is still a literal.
				classFirst = false
				if i < len(src) && src[i] == ']' {
					add(i, i+1, "string")
					i++
				}
				continue
			}
			if strings.HasPrefix(src[i:], "[:") {
				end := i + 2
				for end < len(src) && (src[end] >= 'a' && src[end] <= 'z' || src[end] == '^') {
					end++
				}
				if end+1 < len(src) && src[end:end+2] == ":]" && regexPOSIX(src[i+2:end]) {
					add(i, end+2, "keyword")
					i = end + 2
					classFirst = false
					continue
				}
			}
			switch src[i] {
			case ']':
				if !classFirst {
					add(i, i+1, "delimiter")
					inClass = false
				} else {
					add(i, i+1, "string")
				}
			case '-':
				class := "operator"
				if classFirst || i+1 == len(src) || src[i+1] == ']' {
					class = "string"
				}
				add(i, i+1, class)
			default:
				_, size := utf8.DecodeRuneInString(src[i:])
				add(i, i+size, "string")
				i += size
				classFirst = false
				continue
			}
			i++
			classFirst = false
			continue
		}
		switch src[i] {
		case '[':
			add(i, i+1, "delimiter")
			inClass, classFirst = true, true
			i++
		case '(':
			if !strings.HasPrefix(src[i:], "(?") {
				add(i, i+1, "delimiter")
				i++
				continue
			}
			j := i + 2
			if strings.HasPrefix(src[j:], "P<") || strings.HasPrefix(src[j:], "<") {
				if src[j] == 'P' {
					j++
				}
				j++
				name := j
				for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
					j++
				}
				if j > name && j < len(src) && src[j] == '>' {
					add(i, name, "delimiter")
					add(name, j, "property")
					add(j, j+1, "delimiter")
					i = j + 1
					continue
				}
			} else if j < len(src) && src[j] == ':' {
				add(i, j+1, "delimiter")
				i = j + 1
				continue
			} else {
				for j < len(src) && strings.ContainsRune("imsU-", rune(src[j])) {
					j++
				}
				if j > i+2 && j < len(src) && (src[j] == ':' || src[j] == ')') {
					add(i, i+2, "delimiter")
					for k := i + 2; k < j; k++ {
						class := "keyword"
						if src[k] == '-' {
							class = "delimiter"
						}
						add(k, k+1, class)
					}
					add(j, j+1, "delimiter")
					i = j + 1
					continue
				}
			}
			add(i, len(src), "string")
			return out
		case ')':
			add(i, i+1, "delimiter")
			i++
		case '^', '$':
			add(i, i+1, "keyword")
			i++
		case '.', '|', '*', '+', '?':
			i++
			if src[start] != '.' && src[start] != '|' && i < len(src) && src[i] == '?' {
				i++
			}
			add(start, i, "operator")
		case '{':
			j := i + 1
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			firstEnd := j
			if j < len(src) && src[j] == ',' {
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if firstEnd > i+1 && j < len(src) && src[j] == '}' {
				add(i, i+1, "operator")
				add(i+1, firstEnd, "number")
				if firstEnd < j {
					add(firstEnd, firstEnd+1, "operator")
					add(firstEnd+1, j, "number")
				}
				end := j + 1
				if end < len(src) && src[end] == '?' {
					end++
				}
				add(j, end, "operator")
				i = end
			} else {
				add(i, i+1, "string")
				i++
			}
		default:
			for i < len(src) && !strings.ContainsRune(`\[]()^$.|*+?{`, rune(src[i])) {
				_, size := utf8.DecodeRuneInString(src[i:])
				i += size
			}
			if i == start {
				i++
			}
			add(start, i, "string")
		}
	}
	return out
}

func regexPOSIX(name string) bool {
	name = strings.TrimPrefix(name, "^")
	return strings.Contains("|alnum|alpha|ascii|blank|cntrl|digit|graph|lower|print|punct|space|upper|word|xdigit|", "|"+name+"|")
}

func regexEscape(src string, start int, inClass bool) (int, string, bool) {
	i := start + 1
	if i == len(src) {
		return len(src), "string", false
	}
	c := src[i]
	i++
	if strings.ContainsRune("dDsSwW", rune(c)) {
		return i, "keyword", true
	}
	if !inClass && strings.ContainsRune("AbBz", rune(c)) {
		return i, "keyword", true
	}
	if c == 'p' || c == 'P' {
		name := ""
		if i < len(src) && src[i] == '{' {
			begin := i + 1
			i = begin
			for i < len(src) && src[i] != '}' {
				i++
			}
			if i == len(src) {
				return i, "string", false
			}
			name = strings.TrimPrefix(src[begin:i], "^")
			i++
		} else if i < len(src) {
			name = src[i : i+1]
			i++
		}
		_, category := unicode.Categories[name]
		_, script := unicode.Scripts[name]
		return i, "keyword", name == "Any" || category || script
	}
	if c == 'x' {
		begin, braced := i, i < len(src) && src[i] == '{'
		if braced {
			i++
			begin = i
		}
		for i < len(src) && strings.ContainsRune("0123456789abcdefABCDEF", rune(src[i])) && (braced || i-begin < 2) {
			i++
		}
		if braced {
			if i == begin || i == len(src) || src[i] != '}' {
				return i, "string", false
			}
			i++
		} else if i-begin != 2 {
			return i, "string", false
		}
		return i, "string", true
	}
	if c >= '0' && c <= '7' {
		for i < len(src) && i-start < 4 && src[i] >= '0' && src[i] <= '7' {
			i++
		}
		return i, "string", c == '0' || i-start >= 3
	}
	if strings.ContainsRune("afnrtv", rune(c)) || !inClass && c == 'Q' {
		return i, "string", true
	}
	// Go accepts escaped ASCII punctuation, not backreferences or PCRE escapes.
	return i, "string", c < utf8.RuneSelf && !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
}
