// Package platform abstracts OS-specific behavior so that all platform-dependent
// code is injectable and hermetically testable.
package platform

import (
	"context"
	"io"
)

// Platform abstracts OS-specific behavior.
// Use Real() in production, Fake() in tests.
type Platform interface {
	// TempDir returns the OS temp directory.
	TempDir() string

	// NormalizePath converts OS-native path separators to forward slashes.
	NormalizePath(path string) string

	// AllowedSignals returns the set of signal names valid for wait_for_event
	// with source: signal on this platform.
	AllowedSignals() []string

	// OpenAppend opens (or creates) a file for atomic append writes.
	// On Unix: uses O_APPEND | O_WRONLY | O_CREATE.
	// On Windows: uses best-effort sequential writes with mutex protection.
	OpenAppend(path string) (io.WriteCloser, error)

	// NewlineNormalizer returns a writer that normalizes CRLF → LF.
	// On Unix: returns w unchanged (no-op).
	// On Windows: wraps w with a CRLF stripper.
	NewlineNormalizer(w io.Writer) io.Writer

	// ExecSuffix returns the OS-specific executable suffix ("" on Unix, ".exe" on Windows).
	ExecSuffix() string

	// DefaultShell returns the default shell for script execution.
	// Unix: "/bin/sh". Windows: "cmd.exe".
	DefaultShell() string

	// Exec runs a subprocess and captures its output.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)

	// NotifySignals returns a channel that receives OS signals (SIGTERM, SIGINT).
	// The channel is closed when the context is cancelled.
	// Used by the engine for graceful shutdown handling.
	NotifySignals(ctx context.Context) <-chan Signal
}

// Signal represents an OS signal received by the process.
type Signal struct {
	// Name is the signal name (e.g., "SIGTERM", "SIGINT").
	Name string
}

// ExecRequest describes a subprocess to run.
type ExecRequest struct {
	Command string
	Args    []string
	Env     map[string]string
	Workdir string
	Stdin   string
	Shell   string // override DefaultShell(); empty = use default
}

// ExecResult holds captured subprocess output.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}
