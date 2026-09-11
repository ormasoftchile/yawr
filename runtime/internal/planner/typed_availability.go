package planner

import "github.com/ormasoftchile/yawr/runtime/pkg/schema"

func typedRuntimeAvailability(runbook *schema.Runbook) error {
	return schema.ValidateTypedRunbook(runbook)
}
