package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	toolpkg "github.com/ormasoftchile/yawr/runtime/pkg/tool"
)

func TestRenderArgv_Basic(t *testing.T) {
	argv := []string{"-c", "${count}", "${host}"}
	args := map[string]any{
		"count": "3",
		"host":  "localhost",
	}
	result, err := renderArgv(argv, args)
	if err != nil {
		t.Fatalf("renderArgv failed: %v", err)
	}
	expected := []string{"-c", "3", "localhost"}
	if len(result) != len(expected) {
		t.Fatalf("expected %d args, got %d", len(expected), len(result))
	}
	for i := range result {
		if result[i] != expected[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], result[i])
		}
	}
}

func TestRenderArgv_Empty(t *testing.T) {
	result, err := renderArgv([]string{}, nil)
	if err != nil {
		t.Fatalf("renderArgv failed: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestRenderArgv_BadTemplate(t *testing.T) {
	argv := []string{"${unclosed"}
	_, err := renderArgv(argv, nil)
	if err == nil {
		t.Fatal("expected error for bad template, got nil")
	}
}

func TestNormalizeNativeArgv_WindowsPingCount(t *testing.T) {
	got := normalizeNativeArgv("windows", "ping", []string{"-c", "3", "github.com"})
	want := []string{"-n", "3", "github.com"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d]: expected %q, got %q", i, want[i], got[i])
		}
	}
}

func TestNormalizeNativeArgv_DoesNotRewriteOtherCommands(t *testing.T) {
	got := normalizeNativeArgv("windows", "curl", []string{"-c", "3"})
	if got[0] != "-c" {
		t.Fatalf("expected curl argv to remain unchanged, got %v", got)
	}
}

func TestNativeCLITransport_UnknownAction(t *testing.T) {
	transport := &NativeCLITransport{}
	def := toolpkg.ToolDef{
		Name:    "test",
		Command: "echo",
		Actions: map[string]*toolpkg.ToolAction{
			"known": {
				Argv: []string{"hello"},
			},
		},
	}
	ctx := context.Background()
	_, err := transport.Invoke(ctx, def, "unknown", nil)
	if err == nil {
		t.Fatal("expected error for unknown action, got nil")
	}
}

func TestNativeCLITransport_Echo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX shell utilities (echo, false, /bin/sh)")
	}
	transport := &NativeCLITransport{}
	def := toolpkg.ToolDef{
		Name:    "sh",
		Command: "/bin/sh",
		Actions: map[string]*toolpkg.ToolAction{
			"run": {
				Argv: []string{"-c", "echo hello"},
			},
		},
	}
	ctx := context.Background()
	result, err := transport.Invoke(ctx, def, "run", nil)
	if err != nil {
		t.Fatalf("Invoke failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	t.Logf("Stdout: %q", result.Stdout)
	t.Logf("Stderr: %q", result.Stderr)
	if len(result.Stdout) == 0 {
		t.Errorf("expected non-empty stdout, got empty")
	}
}

func TestNativeCLITransport_PlainNativeCompatibility(t *testing.T) {
	transport := &NativeCLITransport{}
	def := helperNativeToolDef("plain", nil)
	result, err := transport.Invoke(context.Background(), def, "plain", nil)
	if err != nil {
		t.Fatalf("Invoke failed: %v", err)
	}
	if result.Stdout != "plain stdout\n" {
		t.Fatalf("stdout changed: got %q", result.Stdout)
	}
	if result.Stderr != "diagnostic\n" {
		t.Fatalf("stderr changed: got %q", result.Stderr)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code: got %d want 0", result.ExitCode)
	}
	if result.Output != nil {
		t.Fatalf("plain native output changed: got %#v want nil", result.Output)
	}
}

func TestNativeCLITransport_QueryResultContracts(t *testing.T) {
	cases := []struct {
		name     string
		scenario string
		contract *schema.ActionResultContract
		want     map[string]any
	}{
		{
			name:     "CMS shaped envelope",
			scenario: "cms",
			contract: queryResultContract("rowCount", "columns", "data", "metadata"),
			want: map[string]any{
				"success":   true,
				"row_count": int64(1),
				"columns":   []any{"Column1"},
				"rows":      []any{map[string]any{"Column1": jsonNumber("1")}},
				"metadata":  map[string]any{"executedAt": "2026-08-19T18:00:00Z"},
			},
		},
		{
			name:     "Kusto shaped envelope",
			scenario: "kusto",
			contract: queryResultContract("row_count", "schema", "rows", "metadata"),
			want: map[string]any{
				"success":   true,
				"row_count": int64(2),
				"columns":   []any{"TimeGenerated", "Count"},
				"rows":      []any{[]any{"2026-08-19T18:00:00Z", jsonNumber("1")}, []any{"2026-08-19T18:01:00Z", jsonNumber("2")}},
				"metadata":  map[string]any{"cluster": "local"},
			},
		},
		{
			name:     "empty success",
			scenario: "empty",
			contract: queryResultContract("rowCount", "columns", "data", "metadata"),
			want: map[string]any{
				"success":   true,
				"row_count": int64(0),
				"columns":   []any{"Column1"},
				"rows":      []any{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &NativeCLITransport{}
			result, err := transport.Invoke(context.Background(), helperNativeToolDef(tc.scenario, tc.contract), tc.scenario, nil)
			if err != nil {
				t.Fatalf("Invoke failed: %v", err)
			}
			if result.Stdout == "" || result.Stderr != "diagnostic\n" || result.ExitCode != 0 {
				t.Fatalf("process diagnostics not preserved: stdout=%q stderr=%q exit=%d", result.Stdout, result.Stderr, result.ExitCode)
			}
			if !reflect.DeepEqual(result.Output, tc.want) {
				t.Fatalf("output mismatch:\ngot  %#v\nwant %#v", result.Output, tc.want)
			}
		})
	}
}

func TestNativeCLITransport_QueryResultErrors(t *testing.T) {
	cases := []struct {
		name     string
		scenario string
		wantErr  string
	}{
		{"malformed JSON", "malformed", "stdout must be exactly one JSON object"},
		{"missing field", "missing", "missing configured field \"rowCount\""},
		{"wrong mapped type", "wrong-type", "must be an integer"},
		{"multiple JSON objects", "multi", "exactly one JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &NativeCLITransport{}
			result, err := transport.Invoke(context.Background(), helperNativeToolDef(tc.scenario, queryResultContract("rowCount", "columns", "data", "metadata")), tc.scenario, nil)
			if err == nil {
				t.Fatalf("expected error, got nil result=%#v", result)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if result == nil || result.Stdout == "" || result.Stderr != "diagnostic\n" || result.ExitCode != 0 {
				t.Fatalf("process diagnostics not preserved on parse failure: %#v", result)
			}
		})
	}
}

func TestNativeHelperProcess(t *testing.T) {
	if os.Getenv("YAWR_NATIVE_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			emitNativeHelperScenario(args[i+1])
			os.Exit(0)
		}
	}
	fmt.Fprintln(os.Stderr, "missing scenario")
	os.Exit(2)
}

func helperNativeToolDef(scenario string, contract *schema.ActionResultContract) toolpkg.ToolDef {
	return toolpkg.ToolDef{
		Name:    "helper",
		Command: os.Args[0],
		Env:     map[string]string{"YAWR_NATIVE_HELPER": "1"},
		Actions: map[string]*toolpkg.ToolAction{
			scenario: {
				Argv:   []string{"-test.run=TestNativeHelperProcess", "--", scenario},
				Result: contract,
			},
		},
	}
}

func queryResultContract(rowCount, columns, rows, metadata string) *schema.ActionResultContract {
	return &schema.ActionResultContract{
		Format:   schema.ActionResultFormatQueryResultV1,
		Source:   schema.ActionResultSourceStdoutJSON,
		RowCount: rowCount,
		Columns:  columns,
		Rows:     rows,
		Metadata: metadata,
	}
}

func emitNativeHelperScenario(scenario string) {
	fmt.Fprintln(os.Stderr, "diagnostic")
	switch scenario {
	case "plain":
		fmt.Println("plain stdout")
	case "cms":
		fmt.Print(`{"success":true,"rowCount":1,"columns":["Column1"],"data":[{"Column1":1}],"metadata":{"executedAt":"2026-08-19T18:00:00Z"}}`)
	case "kusto":
		fmt.Print(`{"success":true,"row_count":2,"schema":["TimeGenerated","Count"],"rows":[["2026-08-19T18:00:00Z",1],["2026-08-19T18:01:00Z",2]],"metadata":{"cluster":"local"}}`)
	case "empty":
		fmt.Print(`{"success":true,"rowCount":0,"columns":["Column1"],"data":[]}`)
	case "malformed":
		fmt.Print(`{"success":true`)
	case "missing":
		fmt.Print(`{"success":true,"columns":["Column1"],"data":[],"metadata":{}}`)
	case "wrong-type":
		fmt.Print(`{"success":true,"rowCount":"1","columns":["Column1"],"data":[],"metadata":{}}`)
	case "multi":
		fmt.Print(`{"success":true,"rowCount":0,"columns":[],"data":[],"metadata":{}} {}`)
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %s\n", scenario)
		os.Exit(3)
	}
}

func jsonNumber(s string) any { return json.Number(s) }
