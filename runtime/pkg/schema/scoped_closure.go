package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
)

const ScopedFlowClosureVersion = "execution-flow-closure/v4"

// ScopedFlowClosure is the shared integrity envelope. Nodes remain opaque here;
// the planner/snapshot layer validates their executable types and lexical links.
type ScopedFlowClosure struct {
	RootScopeID        string             `json:"root_scope_id"`
	ScopeContextDigest string             `json:"scope_context_digest"`
	SchemaVersion      string             `json:"schema_version"`
	ClosureDigest      string             `json:"closure_digest"`
	Nodes              json.RawMessage    `json:"nodes"`
	Invocation         *RunbookInvocation `json:"invocation,omitempty"`
}

// ScopedFlowClosureDigest canonicalizes object keys, including opaque node
// records, so the shared envelope does not depend on private Go struct layouts.
func ScopedFlowClosureDigest(closure ScopedFlowClosure) (string, error) {
	closure.ClosureDigest = ""
	body, err := json.Marshal(closure)
	if err != nil {
		return "", err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(canonical)), nil
}

func DecodeScopedFlowClosure(body json.RawMessage) (ScopedFlowClosure, error) {
	var closure ScopedFlowClosure
	if len(body) == 0 || len(body) > 64<<20 {
		return closure, fmt.Errorf("scoped closure: missing envelope or size limit exceeded")
	}
	unique := json.NewDecoder(bytes.NewReader(body))
	unique.UseNumber()
	if err := uniqueScopedJSON(unique, 0); err != nil {
		return closure, err
	}
	if _, err := unique.Token(); err != io.EOF {
		return closure, fmt.Errorf("scoped closure: trailing data")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&closure); err != nil {
		return closure, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return closure, fmt.Errorf("scoped closure: trailing data")
	}
	if closure.SchemaVersion != ScopedFlowClosureVersion || closure.RootScopeID == "" ||
		closure.ScopeContextDigest == "" || closure.ClosureDigest == "" {
		return closure, fmt.Errorf("scoped closure: unsupported version or incomplete ownership")
	}
	nodes := bytes.TrimSpace(closure.Nodes)
	if len(nodes) == 0 || nodes[0] != '[' && !bytes.Equal(nodes, []byte("null")) {
		return closure, fmt.Errorf("scoped closure: invalid node table")
	}
	actual, err := ScopedFlowClosureDigest(closure)
	if err != nil {
		return closure, err
	}
	if actual != closure.ClosureDigest {
		return closure, fmt.Errorf("scoped closure: digest mismatch")
	}
	return closure, nil
}

func uniqueScopedJSON(decoder *json.Decoder, depth int) error {
	if depth > 256 {
		return fmt.Errorf("scoped closure: nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("scoped closure: duplicate or invalid object key")
			}
			seen[name] = true
			if err := uniqueScopedJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case json.Delim('['):
		for decoder.More() {
			if err := uniqueScopedJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	return nil
}
