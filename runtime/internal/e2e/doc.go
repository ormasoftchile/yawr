// Package e2e contains end-to-end integration tests for the yawr engine.
// Tests in this package exercise the full stack: parser → planner → engine →
// executors → trace → runstore. They use real file I/O and real subprocess
// execution (echo, cat) and run sequentially (no t.Parallel).
package e2e
