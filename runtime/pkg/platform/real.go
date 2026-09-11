package platform

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

type realPlatform struct{}

// Real returns the production Platform implementation backed by the host OS.
func Real() Platform {
	return &realPlatform{}
}

func (r *realPlatform) TempDir() string {
	return os.TempDir()
}

func (r *realPlatform) NormalizePath(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(path, `\`, "/")
	}
	return path
}

func (r *realPlatform) AllowedSignals() []string {
	switch runtime.GOOS {
	case "linux", "darwin":
		return []string{"SIGINT", "SIGTERM", "SIGUSR1", "SIGUSR2", "SIGHUP"}
	case "windows":
		return []string{"SIGINT"}
	default:
		return []string{"SIGINT"}
	}
}

func (r *realPlatform) OpenAppend(path string) (io.WriteCloser, error) {
	if runtime.GOOS == "windows" {
		// TODO: mutex-protected sequential writes (see decisions.md — Windows Tier 2)
		return os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	}
	return os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
}

func (r *realPlatform) NewlineNormalizer(w io.Writer) io.Writer {
	if runtime.GOOS == "windows" {
		return &crlfWriter{w: w}
	}
	return w
}

func (r *realPlatform) ExecSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func (r *realPlatform) DefaultShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
}

func (r *realPlatform) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errors.New("platform: exec command is required")
	}
	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	if req.Workdir != "" {
		cmd.Dir = req.Workdir
	}
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	if len(req.Env) > 0 {
		envMap := make(map[string]string)
		for _, entry := range os.Environ() {
			if idx := strings.Index(entry, "="); idx != -1 {
				envMap[entry[:idx]] = entry[idx+1:]
			}
		}
		for k, v := range req.Env {
			envMap[k] = v
		}
		env := make([]string, 0, len(envMap))
		for k, v := range envMap {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}
	return &ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// NotifySignals registers for SIGTERM and SIGINT and returns a channel that
// receives Signal values when those signals are caught.
// The channel is closed when ctx is cancelled.
func (r *realPlatform) NotifySignals(ctx context.Context) <-chan Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	out := make(chan Signal, 1)
	go func() {
		defer close(out)
		defer signal.Stop(sigCh)
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				out <- Signal{Name: sig.String()}
			}
		}
	}()
	return out
}

// crlfWriter replaces \r\n with \n before writing to the underlying writer.
type crlfWriter struct {
	w io.Writer
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	normalized := bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	_, err := c.w.Write(normalized)
	// Report the original length to callers; the transformation is transparent.
	return len(p), err
}
