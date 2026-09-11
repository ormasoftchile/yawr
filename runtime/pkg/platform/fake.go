package platform

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// FakePlatform is a controllable Platform for use in tests.
// All behavior is configurable via exported fields.
type FakePlatform struct {
	TempDirPath     string
	Signals         []string
	ExecSuffixStr   string
	ShellPath       string
	AppendCallCount int

	// ExecResults maps command → canned ExecResult.
	ExecResults map[string]*ExecResult
	// DefaultExecResult is returned when no command match exists.
	DefaultExecResult *ExecResult
	// ExecRequests records all Exec invocations.
	ExecRequests []ExecRequest
	// ExecError forces Exec to return this error.
	ExecError error
	mu        sync.Mutex

	// AppendBuf accumulates all bytes written via OpenAppend across calls.
	AppendBuf bytes.Buffer

	// SignalCh is optionally set by tests to control NotifySignals output.
	// If nil, NotifySignals returns a closed channel.
	SignalCh chan Signal
}

// NewFakePlatform returns a *FakePlatform with sensible Unix defaults.
func NewFakePlatform() *FakePlatform {
	return &FakePlatform{
		TempDirPath:   "/tmp",
		Signals:       []string{"SIGINT", "SIGTERM", "SIGUSR1", "SIGUSR2", "SIGHUP"},
		ExecSuffixStr: "",
		ShellPath:     "/bin/sh",
	}
}

func (f *FakePlatform) TempDir() string {
	return f.TempDirPath
}

func (f *FakePlatform) NormalizePath(path string) string {
	return path
}

func (f *FakePlatform) AllowedSignals() []string {
	return f.Signals
}

// OpenAppend returns a WriteCloser backed by FakePlatform.AppendBuf.
// Each call increments AppendCallCount.
func (f *FakePlatform) OpenAppend(_ string) (io.WriteCloser, error) {
	f.AppendCallCount++
	return &fakeWriteCloser{buf: &f.AppendBuf}, nil
}

func (f *FakePlatform) NewlineNormalizer(w io.Writer) io.Writer {
	return w
}

func (f *FakePlatform) ExecSuffix() string {
	return f.ExecSuffixStr
}

func (f *FakePlatform) DefaultShell() string {
	return f.ShellPath
}

// Exec returns a canned ExecResult for the command, or DefaultExecResult when set.
func (f *FakePlatform) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	f.mu.Lock()
	f.ExecRequests = append(f.ExecRequests, req)
	f.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.ExecError != nil {
		return nil, f.ExecError
	}
	if f.ExecResults != nil {
		if res, ok := f.ExecResults[req.Command]; ok {
			return res, nil
		}
	}
	if f.DefaultExecResult != nil {
		return f.DefaultExecResult, nil
	}
	return &ExecResult{ExitCode: 0}, nil
}

// NotifySignals returns f.SignalCh if set, otherwise an immediately closed channel.
// Tests can set f.SignalCh to inject signals.
func (f *FakePlatform) NotifySignals(ctx context.Context) <-chan Signal {
	if f.SignalCh != nil {
		return f.SignalCh
	}
	ch := make(chan Signal)
	close(ch)
	return ch
}

// fakeWriteCloser wraps a *bytes.Buffer to satisfy io.WriteCloser.
type fakeWriteCloser struct {
	buf *bytes.Buffer
}

func (fw *fakeWriteCloser) Write(p []byte) (int, error) {
	return fw.buf.Write(p)
}

func (fw *fakeWriteCloser) Close() error {
	return nil
}
