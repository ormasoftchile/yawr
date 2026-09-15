package parser

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

// DecodeDependencyMetadata uses the runtime's YAML shape decoder without
// authorizing execution or requiring a complete executable editor buffer.
func DecodeDependencyMetadata(source []byte) (*schema.Runbook, error) {
	return parseRunbook(source)
}
