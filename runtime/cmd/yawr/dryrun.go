package main

import "github.com/ormasoftchile/yawr/runtime/pkg/engine"

func runDryRun(args []string) int {
	return runWithMode(args, engine.RunModeDryRun)
}
