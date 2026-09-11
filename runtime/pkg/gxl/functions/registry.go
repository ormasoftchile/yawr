// Package functions defines the finite, unevaluated GXL builtin inventory.
package functions

import "strings"

type Definition struct {
	Name       string
	Label      string
	Comparator bool
}

var definitions = []Definition{
	{"len", "len(value: string | list) -> number", true},
	{"now", "now() -> string", false},
	{"str.startsWith", "str.startsWith(text: string, prefix: string) -> boolean", true},
	{"str.endsWith", "str.endsWith(text: string, suffix: string) -> boolean", true},
	{"str.contains", "str.contains(text: string, substring: string) -> boolean", true},
	{"str.toLower", "str.toLower(text: string) -> string", true},
	{"str.toUpper", "str.toUpper(text: string) -> string", true},
	{"str.trim", "str.trim(text: string) -> string", true},
	{"str.length", "str.length(value: string | list) -> number", true},
	{"str.trimPrefix", "str.trimPrefix(text: string, prefix: string) -> string", true},
	{"str.trimSuffix", "str.trimSuffix(text: string, suffix: string) -> string", true},
	{"list.contains", "list.contains(items: list, item: scalar) -> boolean", true},
	{"list.indexOf", "list.indexOf(items: list, item: scalar) -> number", true},
	{"list.length", "list.length(items: list) -> number", true},
	{"list.order", "list.order(items: list, comparator: expression-string) -> list", false},
	{"regex.match", "regex.match(text: string, pattern: string) -> boolean", true},
	{"date.compare", "date.compare(left: string, right: string) -> number", true},
	{"date.diffSeconds", "date.diffSeconds(left: string, right: string) -> number", true},
}

func All() []Definition { return append([]Definition(nil), definitions...) }
func Lookup(name string) (Definition, bool) {
	for _, d := range definitions {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}
func Namespace(name string) bool {
	for _, d := range definitions {
		if strings.HasPrefix(d.Name, name+".") {
			return true
		}
	}
	return false
}
func (d Definition) Parameters() []string {
	p := d.Label[strings.IndexByte(d.Label, '(')+1 : strings.IndexByte(d.Label, ')')]
	if p == "" {
		return []string{}
	}
	return strings.Split(p, ", ")
}
