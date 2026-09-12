package graphdoc_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/parser"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

// TestDocument_HashStable verifies that two independent builds of the
// same runbook produce identical hashes. The hash is the canonical
// fingerprint we ship over /preview/document so caches and ETags depend
// on it being deterministic.
func TestDocument_HashStable(t *testing.T) {
	build := func() *graphdoc.Document {
		p, err := parser.New(platform.Real())
		if err != nil {
			t.Fatalf("parser.New: %v", err)
		}
		rb, err := p.Parse(context.Background(), collectHealthPath())
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		doc, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), rb)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return doc
	}

	a := build()
	b := build()
	if a.Hash == "" {
		t.Fatal("hash is empty")
	}
	if a.Hash != b.Hash {
		t.Errorf("hash unstable across builds:\n  a=%s\n  b=%s", a.Hash, b.Hash)
	}
}

func TestDocument_HashIncludesVarsAndExcludesCheckoutPath(t *testing.T) {
	build := func(directory, route string) string {
		t.Helper()
		path := filepath.Join(directory, "route.runbook.yaml")
		source := "apiVersion: yawr.runbook/v1\nid: route\nname: Route\nkind: mitigation\nvars:\n  route_value: " + route + "\nflow:\n  - step:\n      id: done\n      type: end\n      outcome: { category: succeeded, code: done }\n"
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		parserImpl, err := parser.New(platform.Real())
		if err != nil {
			t.Fatalf("parser.New: %v", err)
		}
		runbook, err := parserImpl.Parse(context.Background(), path)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		document, err := (&graphdoc.Builder{}).Build(context.Background(), runbook)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return document.Hash
	}

	first := build(t.TempDir(), "primary")
	relocated := build(t.TempDir(), "primary")
	changed := build(t.TempDir(), "secondary")
	if first != relocated {
		t.Fatalf("hash changed across checkout locations: %s != %s", first, relocated)
	}
	if first == changed {
		t.Fatalf("hash ignored route-affecting vars: %s", first)
	}
}

// TestDocument_HashChangesWhenStructureChanges verifies that mutating any
// flow-node ID produces a different hash. This is a sanity check that
// the hash actually covers content (not, say, only Runbook.ID).
func TestDocument_HashChangesWhenStructureChanges(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	docA, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build A: %v", err)
	}
	if len(rb.Runbook.Flow) == 0 {
		t.Fatalf("expected at least one flow node in fixture")
	}
	// Mutate the first node's title (it has a Step in this fixture).
	if rb.Runbook.Flow[0].Step != nil {
		rb.Runbook.Flow[0].Step.ID = rb.Runbook.Flow[0].Step.ID + "_mutated"
	} else if rb.Runbook.Flow[0].Iterate != nil {
		rb.Runbook.Flow[0].Iterate.ID = rb.Runbook.Flow[0].Iterate.ID + "_mutated"
	} else if rb.Runbook.Flow[0].Parallel != nil {
		rb.Runbook.Flow[0].Parallel.ID = rb.Runbook.Flow[0].Parallel.ID + "_mutated"
	} else {
		t.Fatalf("first flow node has no recognized variant")
	}
	docB, err := (&graphdoc.Builder{Loader: &fileLoader{p: p}}).Build(context.Background(), rb)
	if err != nil {
		t.Fatalf("Build B: %v", err)
	}
	if docA.Hash == docB.Hash {
		t.Errorf("hash did not change when a flow node ID was mutated: %s", docA.Hash)
	}
}

// TestDocument_HashIgnoresMapInsertionOrder verifies that internal map
// iteration order does not leak into the hash. Build the same doc
// many times and confirm a single distinct hash.
func TestDocument_HashIgnoresMapInsertionOrder(t *testing.T) {
	p, err := parser.New(platform.Real())
	if err != nil {
		t.Fatalf("parser.New: %v", err)
	}
	rb, err := p.Parse(context.Background(), collectHealthPath())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	loader := &fileLoader{p: p}
	hashes := make(map[string]int)
	for i := 0; i < 20; i++ {
		doc, err := (&graphdoc.Builder{Loader: loader}).Build(context.Background(), rb)
		if err != nil {
			t.Fatalf("Build %d: %v", i, err)
		}
		hashes[doc.Hash]++
	}
	if len(hashes) != 1 {
		t.Errorf("expected 1 distinct hash across 20 builds, got %d: %v", len(hashes), hashes)
	}
}
