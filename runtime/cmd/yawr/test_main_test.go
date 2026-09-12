package main

import (
	"os"
	"testing"
)

// TestMain configures the test binary before any test runs.
//
// TTY simulation: existing CLI tests in this package call runRun() without
// --profile because they test features unrelated to profile-based execution
// (package-map wiring, enum validation, etc.). The profileless fail-fast
// check (Item 5) would fire for all of them because `go test` runs with
// stdin connected to a pipe, not a terminal. To avoid this pervasive
// breakage, TestMain replaces interactiveTTYDetect with a stub that reports
// true (simulating an attended terminal) for the duration of the test binary.
//
// Tests that specifically exercise the fail-fast path (e.g.
// TestCLI_Profileless_NonInteractive_FailFast) temporarily restore the real
// detector inside their test body via t.Cleanup.
func TestMain(m *testing.M) {
	// Simulate an attended terminal for all tests that don't specifically
	// exercise the non-interactive fail-fast path.
	interactiveTTYDetect = func() bool { return true }
	runDir, err := os.MkdirTemp("", "yawr-cli-test-runs-")
	if err != nil {
		panic(err)
	}
	defaultRunStoreDir = runDir
	code := m.Run()
	_ = os.RemoveAll(runDir)
	os.Exit(code)
}
