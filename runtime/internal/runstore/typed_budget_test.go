package runstore

import (
	"fmt"
	"strings"
	"testing"
)

func TestTypedRegistryUsesExistingStorageBudgets(t *testing.T) {
	if maxStoredValueEntries != 4096 || maxRunStateBytes != 16<<20 || maxCheckpointExpandedBytes != 256<<20 {
		t.Fatal("existing budgets changed")
	}
	values := make(map[string]any, maxStoredValueEntries+1)
	for index := 0; index <= maxStoredValueEntries; index++ {
		values[fmt.Sprint(index)] = nil
	}
	if err := validateStateValueBudgets(nil, nil, nil, nil, values); err == nil {
		t.Fatal("typed registry bypassed slot budget")
	}
	stored := map[string]storedJSONValueV1{}
	for index := 0; index < 5; index++ {
		stored[fmt.Sprint(index)] = storedJSONValueV1{Blob: &stateBlobRefV1{Digest: fmt.Sprintf("sha256:%064x", index+1), Encoding: "json+gzip", UncompressedSize: 64 << 20}}
	}
	loader := &stateValueLoader{}
	if err := loader.preflight(nil, nil, nil, nil, stored); err == nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("typed registry bypassed expanded budget: %v", err)
	}
}
