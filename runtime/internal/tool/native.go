package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ormasoftchile/yawr/runtime/pkg/gis"
	gxleval "github.com/ormasoftchile/yawr/runtime/pkg/gxl/eval"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

// NativeCLITransport spawns a native binary with argv per invocation.
// Does NOT speak JSON protocol; captures stdout/stderr as plain text.
type NativeCLITransport struct{}

// Invoke runs the native CLI tool with rendered argv and returns captured output.
func (t *NativeCLITransport) Invoke(ctx context.Context, def toolpkg.ToolDef, action string, args map[string]any) (*toolpkg.ToolResult, error) {
	actionDef, ok := def.Actions[action]
	if !ok {
		return nil, fmt.Errorf("native tool %s: action %q not found", def.Name, action)
	}

	// Render argv templates with args as data
	renderedArgv, err := renderArgv(actionDef.Argv, args)
	if err != nil {
		return nil, fmt.Errorf("native tool %s action %s: render argv: %w", def.Name, action, err)
	}
	renderedArgv = normalizeNativeArgv(runtime.GOOS, def.Command, renderedArgv)

	// Start the process
	proc, err := StartProcess(ctx, def.Command, renderedArgv, def.Env)
	if err != nil {
		return nil, fmt.Errorf("native tool %s: start process: %w", def.Name, err)
	}

	// No stdin write for native tools (they don't read JSON)
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
			return nil, fmt.Errorf("native tool %s: wait: %w", def.Name, waitErr)
		}
	}

	stdout := string(stdoutBuf)
	result := &toolpkg.ToolResult{
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   string(stderrBuf),
		Output:   nil, // Native tools emit plain text unless an action result contract opts in.
	}

	// Non-zero exit is an error for native tools. Result parsing is only for
	// successful process exits so stderr/exit diagnostics stay authoritative.
	if exitCode != 0 {
		return result, fmt.Errorf("native tool %s exited with code %d", def.Name, exitCode)
	}
	if actionDef.Result != nil {
		output, err := parseNativeActionResult(stdout, actionDef.Result)
		if err != nil {
			return result, fmt.Errorf("native tool %s action %s: result: %w", def.Name, action, err)
		}
		result.Output = output
	}
	return result, nil
}

func parseNativeActionResult(stdout string, contract *schema.ActionResultContract) (map[string]any, error) {
	if contract.Format != schema.ActionResultFormatQueryResultV1 {
		return nil, fmt.Errorf("unsupported format %q", contract.Format)
	}
	if contract.Source != schema.ActionResultSourceStdoutJSON {
		return nil, fmt.Errorf("unsupported source %q", contract.Source)
	}

	dec := json.NewDecoder(bytes.NewBufferString(stdout))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("stdout must be exactly one JSON object: %w", err)
	}
	if obj == nil {
		return nil, fmt.Errorf("stdout must be exactly one JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("stdout must contain exactly one JSON object")
		}
		return nil, fmt.Errorf("stdout must contain exactly one JSON object: %w", err)
	}

	successRaw, ok := obj["success"]
	if !ok {
		return nil, fmt.Errorf("missing required field %q", "success")
	}
	success, ok := successRaw.(bool)
	if !ok {
		return nil, fmt.Errorf("field %q must be boolean", "success")
	}
	rowCount, err := requireJSONInteger(obj, contract.RowCount, "row_count")
	if err != nil {
		return nil, err
	}
	columns, err := requireStringArray(obj, contract.Columns, "columns")
	if err != nil {
		return nil, err
	}
	rows, err := requireArray(obj, contract.Rows, "rows")
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"success":   success,
		"row_count": rowCount,
		"columns":   columns,
		"rows":      rows,
	}
	if contract.Metadata != "" {
		if raw, ok := obj[contract.Metadata]; ok && raw != nil {
			metadata, err := requireObject(obj, contract.Metadata, "metadata")
			if err != nil {
				return nil, err
			}
			out["metadata"] = metadata
		}
	}
	return out, nil
}

func requireJSONInteger(obj map[string]any, sourceField, outputName string) (int64, error) {
	raw, ok := obj[sourceField]
	if !ok {
		return 0, fmt.Errorf("missing configured field %q for %s", sourceField, outputName)
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("configured field %q for %s must be an integer", sourceField, outputName)
	}
	i, err := n.Int64()
	if err != nil {
		return 0, fmt.Errorf("configured field %q for %s must be an integer", sourceField, outputName)
	}
	return i, nil
}

func requireStringArray(obj map[string]any, sourceField, outputName string) ([]any, error) {
	raw, ok := obj[sourceField]
	if !ok {
		return nil, fmt.Errorf("missing configured field %q for %s", sourceField, outputName)
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("configured field %q for %s must be an array", sourceField, outputName)
	}
	out := make([]any, len(arr))
	for i, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("configured field %q for %s must be an array of strings", sourceField, outputName)
		}
		out[i] = s
	}
	return out, nil
}

func requireArray(obj map[string]any, sourceField, outputName string) ([]any, error) {
	raw, ok := obj[sourceField]
	if !ok {
		return nil, fmt.Errorf("missing configured field %q for %s", sourceField, outputName)
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("configured field %q for %s must be an array", sourceField, outputName)
	}
	return arr, nil
}

func requireObject(obj map[string]any, sourceField, outputName string) (map[string]any, error) {
	raw, ok := obj[sourceField]
	if !ok {
		return nil, fmt.Errorf("missing configured field %q for %s", sourceField, outputName)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("configured field %q for %s must be an object", sourceField, outputName)
	}
	return m, nil
}

// Close is a no-op for native transport (stateless, one-shot invocations).
func (t *NativeCLITransport) Close() error {
	return nil
}

// renderArgv renders each argv element as a GIS template with args as the scope.
// Tool-arg names are bound at top-level scope, so `${url}` resolves to args["url"].
func renderArgv(argv []string, args map[string]any) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	scope, err := gxleval.FromAny(args)
	if err != nil {
		return nil, fmt.Errorf("build argv scope: %w", err)
	}
	out := make([]string, 0, len(argv))
	for i, tmplStr := range argv {
		rendered, err := gis.Interpolate(tmplStr, scope)
		if err != nil {
			return nil, fmt.Errorf("argv[%d]: %w", i, err)
		}
		out = append(out, rendered)
	}
	return out, nil
}

func normalizeNativeArgv(goos, command string, argv []string) []string {
	if goos != "windows" || !strings.EqualFold(strings.TrimSuffix(filepath.Base(command), filepath.Ext(command)), "ping") {
		return argv
	}

	normalized := append([]string(nil), argv...)
	for i, arg := range normalized {
		if arg == "-c" {
			normalized[i] = "-n"
		}
	}
	return normalized
}
