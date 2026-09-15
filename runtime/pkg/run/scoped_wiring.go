package run

import (
	internalexecutor "github.com/ormasoftchile/yawr/runtime/internal/executor"
)

type scopedRunWiring struct {
	loader   internalexecutor.LazyRunbookLoader
	resolver internalexecutor.DynamicIncludeResolver
}
