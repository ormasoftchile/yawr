package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"

	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// StdioTransport spawns a process per invocation and communicates via stdio.
type StdioTransport struct{}

// Invoke runs the tool via stdio and returns the captured output.
func (t *StdioTransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	proc, err := StartProcess(ctx, def.Command, def.Args, def.Env)
	if err != nil {
		return nil, err
	}

	req := map[string]any{
		"action": action,
		"args":   args,
	}
	if err := json.NewEncoder(proc.stdin).Encode(req); err != nil {
		_ = proc.stdin.Close()
		_ = proc.Kill()
		return nil, err
	}
	_ = proc.stdin.Close()

	stdoutBuf, stderrBuf, waitErr := proc.readAllAndWait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, waitErr
		}
	}

	result := &toolpkg.ToolResult{
		ExitCode: exitCode,
		Stdout:   string(stdoutBuf),
		Stderr:   string(stderrBuf),
	}

	var parsed map[string]any
	if len(bytes.TrimSpace(stdoutBuf)) > 0 && json.Unmarshal(stdoutBuf, &parsed) == nil {
		result.Output = parsed
	}

	if exitCode != 0 {
		return result, fmt.Errorf("tool exited with code %d", exitCode)
	}
	return result, nil
}

// Close is a no-op for stdio transport.
func (t *StdioTransport) Close() error {
	return nil
}
