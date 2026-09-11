package gdp

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/pjvm"
)

func TestResolve(t *testing.T) {
	zero := 0
	tree, err := pjvm.FromAny(map[string]any{"foo": map[string]any{"items": []any{map[string]any{"name": "first"}}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(tree, &Path{Dialect: DialectGXL, Root: "foo", Segments: []Segment{{Name: "items"}, {Index: &zero}, {Name: "name"}}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want, _ := pjvm.NewString("first")
	if !got.Equal(want) {
		t.Fatalf("Resolve() = %s, want %s", got.String(), want.String())
	}
}

func TestResolveOptional(t *testing.T) {
	idx := 0
	tree, err := pjvm.FromAny(map[string]any{"user": map[string]any{}, "items": nil})
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := ResolveOptional(tree, &Path{Dialect: DialectGXL, Root: "user", Segments: []Segment{{Name: "email", Optional: true}, {Name: "domain"}}})
	if err != nil {
		t.Fatalf("ResolveOptional() error = %v", err)
	}
	if found {
		t.Fatal("ResolveOptional() found = true, want false")
	}
	_, found, err = ResolveOptional(tree, &Path{Dialect: DialectGXL, Root: "items", Segments: []Segment{{Index: &idx, Optional: true}}})
	if err != nil {
		t.Fatalf("ResolveOptional() null index error = %v", err)
	}
	if found {
		t.Fatal("ResolveOptional() null index found = true, want false")
	}
}
