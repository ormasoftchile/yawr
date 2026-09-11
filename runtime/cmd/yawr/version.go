package main

import (
	"fmt"
	"os"
)

// Build-time version variables set via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func runVersion(_ []string) int {
	fmt.Fprintf(os.Stdout, "yawr %s (commit %s) built %s\n", Version, Commit, BuildDate)
	return exitSuccess
}
