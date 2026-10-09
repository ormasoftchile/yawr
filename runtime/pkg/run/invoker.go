package run

import (
	_ "github.com/ormasoftchile/yawr/runtime/internal/tool" // registers invoker factory
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// NewToolInvoker constructs a ready-to-use ToolInvoker with YAWR's production defaults.
// This is the primary public entry point for external Go applications (such as A4C)
// to invoke registered tools with governance, durable intent, and evidence collection.
func NewToolInvoker(opts tool.InvokerOptions) (tool.ToolInvoker, error) {
	return tool.NewInvoker(opts)
}

// NewPersistentDispatchStore constructs a persistent, file-backed dispatch store in dir
// with cross-process locking, crash recovery, and idempotency guarantees.
func NewPersistentDispatchStore(dir string) (any, error) {
	return internaltool.NewFileDispatchStore(dir)
}
