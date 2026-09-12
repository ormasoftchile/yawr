package graphdoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/presentation"
)

func TestMetadataFreeEmptyGraphHashRemainsUnchanged(t *testing.T) {
	for _, nodes := range [][]Node{nil, {}} {
		doc := &Document{Nodes: nodes}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		actual, err := doc.ContentHash()
		if err != nil || actual != hex.EncodeToString(sum[:]) {
			t.Fatalf("metadata-free hash changed: %s, %v", actual, err)
		}
	}
}

func TestPresentationHashExcludesOnlyTypedSelfBinding(t *testing.T) {
	details := &StepDetails{Kind: "tool", CodePresentation: &presentation.Envelope{
		Version: 1, Origin: "frozen", PlanSnapshotDigest: "first",
	}}
	doc := &Document{Nodes: []Node{{ID: "code", Details: details}}}
	before, err := doc.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	details.CodePresentation.PlanSnapshotDigest = "second"
	after, _ := doc.ContentHash()
	if before != after || details.CodePresentation.PlanSnapshotDigest != "second" {
		t.Fatal("self-binding was hashed or mutated")
	}
	details.CodePresentation.ToolID = "other"
	after, _ = doc.ContentHash()
	if before == after {
		t.Fatal("tool identity excluded")
	}
	details.CodePresentation = nil
	details.Arguments = []NamedDetailValue{{Name: "data", Value: map[string]any{"plan_snapshot_digest": "first"}}}
	before, _ = doc.ContentHash()
	details.Arguments[0].Value = map[string]any{"plan_snapshot_digest": "second"}
	after, _ = doc.ContentHash()
	if before == after {
		t.Fatal("arbitrary user argument key excluded")
	}
}
