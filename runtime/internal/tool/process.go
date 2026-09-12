package tool

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

// ProcessHandle wraps a running tool process and its I/O pipes.
type ProcessHandle struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan struct{}

	waitOnce sync.Once
	waitErr  error
}

// StartProcess starts a command with pipes configured and context cancellation.
func StartProcess(ctx context.Context, command string, args []string, env map[string]string) (*ProcessHandle, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = mergeEnv(env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	h := &ProcessHandle{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan struct{}),
	}

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = h.Kill()
			case <-h.done:
			}
		}()
	}

	return h, nil
}

// Wait blocks until the process exits and returns its exit error if any.
func (p *ProcessHandle) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	})
	<-p.done
	return p.waitErr
}

func (p *ProcessHandle) readAllAndWait() ([]byte, []byte, error) {
	var readers sync.WaitGroup
	readers.Add(2)
	var stdout []byte
	var stderr []byte
	go func() {
		defer readers.Done()
		stdout, _ = io.ReadAll(p.stdout)
	}()
	go func() {
		defer readers.Done()
		stderr, _ = io.ReadAll(p.stderr)
	}()
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()
	readers.Wait()
	return stdout, stderr, <-waited
}

// Kill terminates the underlying process.
func (p *ProcessHandle) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func mergeEnv(env map[string]string) []string {
	out := append([]string{}, os.Environ()...)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
