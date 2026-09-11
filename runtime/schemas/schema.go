// Package schemas exports the embedded JSON Schema files for yawr documents.
package schemas

import _ "embed"

// RunbookSchema is the JSON Schema (Draft 2020-12) for a yawr runbook document.
//
//go:embed runbook.schema.json
var RunbookSchema []byte
