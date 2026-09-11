// Package sensitive defines conservative names that must not carry persisted secrets.
package sensitive

import (
	"strings"
	"unicode"
)

var nameTokens = map[string]bool{
	"secret": true, "password": true, "passwd": true, "token": true,
	"credential": true, "key": true, "authorization": true, "auth": true,
}

// Name reports whether a structured field name contains a sensitive token.
func Name(name string) bool {
	for _, token := range Tokens(name) {
		if nameTokens[token] {
			return true
		}
	}
	return false
}

// Tokens splits snake, kebab, spaced, and camel-case names consistently.
func Tokens(name string) []string {
	runes := []rune(name)
	result := make([]string, 0, 4)
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		result = append(result, token.String())
		token.Reset()
	}
	for index, current := range runes {
		if !unicode.IsLetter(current) && !unicode.IsDigit(current) {
			flush()
			continue
		}
		if unicode.IsUpper(current) && token.Len() > 0 {
			previous := runes[index-1]
			nextIsLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || unicode.IsUpper(previous) && nextIsLower {
				flush()
			}
		}
		token.WriteRune(unicode.ToLower(current))
	}
	flush()
	return result
}
