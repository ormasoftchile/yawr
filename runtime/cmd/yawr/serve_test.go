package main

import (
	"testing"
)

func TestServeFlags_CorsOrigin_Repeatable(t *testing.T) {
	t.Parallel()

	var f repeatedStringFlag
	if err := f.Set("https://app.example.com"); err != nil {
		t.Fatalf("Set A: %v", err)
	}
	if err := f.Set("https://staging.example.com"); err != nil {
		t.Fatalf("Set B: %v", err)
	}
	origins := []string(f)
	if len(origins) != 2 {
		t.Fatalf("expected 2 origins, got %d", len(origins))
	}
	if origins[0] != "https://app.example.com" {
		t.Errorf("origins[0]: expected https://app.example.com, got %s", origins[0])
	}
	if origins[1] != "https://staging.example.com" {
		t.Errorf("origins[1]: expected https://staging.example.com, got %s", origins[1])
	}
}
