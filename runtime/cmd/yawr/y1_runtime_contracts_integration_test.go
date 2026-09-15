package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

const y1HelperEnvironment = "YAWR_Y1_RUNTIME_HELPER"

func TestY1RuntimeContractsHelper(t *testing.T) {
	if os.Getenv(y1HelperEnvironment) != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(2)
	}
	args := os.Args[separator+1:]
	switch args[0] {
	case "run":
		os.Exit(runRun(args[1:]))
	case "preview":
		os.Exit(runPreview(args[1:]))
	case "serve":
		os.Exit(runServe(args[1:]))
	case "counter":
		if len(args) != 2 {
			os.Exit(2)
		}
		count := 0
		if data, err := os.ReadFile(args[1]); err == nil {
			count, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if err := os.WriteFile(args[1], []byte(strconv.Itoa(count+1)), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(`{"ok":true}`)
		os.Exit(0)
	case "large":
		fmt.Print(strings.Repeat("á😀", 200000))
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

type y1DirectSummary struct {
	RunID              string                       `json:"run_id"`
	Status             string                       `json:"status"`
	Results            *engine.RunResults           `json:"results"`
	ResultsUnavailable *resultsdelivery.Unavailable `json:"results_unavailable"`
}

func y1Fixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "y1-runtime-contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func y1Command(t *testing.T, mode string, args ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandArgs := append([]string{"-test.run=^TestY1RuntimeContractsHelper$", "--", mode}, args...)
	cmd := exec.Command(executable, commandArgs...)
	cmd.Env = append(os.Environ(), y1HelperEnvironment+"=1")
	return cmd
}

func y1RunCommand(t *testing.T, args ...string) ([]byte, []byte, error) {
	t.Helper()
	cmd := y1Command(t, "run", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func y1ReadCounter(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func y1AssertNativeResults(t *testing.T, record *engine.RunResults) {
	t.Helper()
	if record == nil {
		t.Fatal("missing canonical Results")
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"flag":       false,
		"zero":       json.Number("0"),
		"empty_text": "",
		"empty_list": []any{},
		"empty_map":  map[string]any{},
	}
	for name, expected := range want {
		actual := record.Outputs[name].Value
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("%s=%#v, want %#v", name, actual, expected)
		}
	}
	result := record.Outputs["result"].Value.(map[string]any)
	if result["unicode"] != "Español 😀" {
		t.Fatalf("unicode changed: %#v", result)
	}
	nested := result["nested"].(map[string]any)
	if value, exists := nested["present_null"]; !exists || value != nil {
		t.Fatalf("present nested null lost: %#v", nested)
	}
	values := nested["values"].([]any)
	if len(values) != 5 || values[0] != false || values[1] != json.Number("0") || values[2] != "" ||
		len(values[3].([]any)) != 0 || len(values[4].(map[string]any)) != 0 {
		t.Fatalf("native nested values changed: %#v", values)
	}
}

func TestY1RuntimeContractsDirectPersistedResume(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs")
	counter := filepath.Join(dir, "counter.txt")
	executable, _ := os.Executable()
	stdout, stderr, err := y1RunCommand(t, y1Fixture(t, "native-values.runbook.yaml"),
		"--require-capabilities", "yawr.typed-results/v1,yawr.run-get-results/v1",
		"--run-dir", runDir, "--output", "json",
		"--var", "helper_path="+executable, "--var", "counter_path="+counter)
	if err != nil {
		t.Fatalf("direct run: %v\nstderr=%s\nstdout=%s", err, stderr, stdout)
	}
	var direct y1DirectSummary
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	decoder.UseNumber()
	if err := decoder.Decode(&direct); err != nil {
		t.Fatal(err)
	}
	if direct.RunID == "" || direct.Status != "completed" {
		t.Fatalf("incomplete direct envelope: %s", stdout)
	}
	y1AssertNativeResults(t, direct.Results)
	if y1ReadCounter(t, counter) != 1 {
		t.Fatal("producer did not run exactly once")
	}

	resumedOut, resumedErr, err := y1RunCommand(t, "--resume", direct.RunID, "--run-dir", runDir, "--output", "json")
	if err != nil {
		t.Fatalf("resume: %v\nstderr=%s\nstdout=%s", err, resumedErr, resumedOut)
	}
	var resumed y1DirectSummary
	decoder = json.NewDecoder(bytes.NewReader(resumedOut))
	decoder.UseNumber()
	if err := decoder.Decode(&resumed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(direct.Results, resumed.Results) || resumed.RunID != direct.RunID {
		t.Fatalf("resume changed committed publication:\ndirect=%s\nresume=%s", stdout, resumedOut)
	}
	if y1ReadCounter(t, counter) != 1 {
		t.Fatal("resume reran producer")
	}

	base, stop := y1StartServer(t, dir, runDir)
	defer stop()
	persisted := y1RPC(t, base, "run.get", map[string]any{"runID": direct.RunID})
	result := persisted["result"].(map[string]any)
	var fromServer engine.RunResults
	body, _ := json.Marshal(result["results"])
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&fromServer); err != nil {
		t.Fatal(err)
	}
	if result["source"] != "persisted" || !reflect.DeepEqual(direct.Results, &fromServer) {
		t.Fatalf("persisted run.get changed publication: %#v", result)
	}
	if y1ReadCounter(t, counter) != 1 {
		t.Fatal("persisted read reran producer")
	}
}

func TestY1RuntimeContractsOpenStdinAndServedExecution(t *testing.T) {
	dir := t.TempDir()
	executable, _ := os.Executable()
	counter := filepath.Join(dir, "stdio-counter.txt")
	cmd := y1Command(t, "run", y1Fixture(t, "native-values.runbook.yaml"), "--stdio",
		"--require-capabilities", "yawr.typed-results/v1,yawr.run-results-chunks/v1",
		"--run-dir", filepath.Join(dir, "stdio-runs"),
		"--var", "helper_path="+executable, "--var", "counter_path="+counter)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("stdio run: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
	}
	var terminal map[string]json.RawMessage
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		if string(frame["type"]) == `"run.finished"` {
			terminal = frame
		}
	}
	if terminal == nil {
		t.Fatal("missing actual run.finished frame")
	}
	var stdioResults engine.RunResults
	decoder := json.NewDecoder(bytes.NewReader(terminal["results"]))
	decoder.UseNumber()
	if err := decoder.Decode(&stdioResults); err != nil {
		t.Fatal(err)
	}
	y1AssertNativeResults(t, &stdioResults)
	if y1ReadCounter(t, counter) != 1 {
		t.Fatal("stdio producer count differs")
	}

	base, stop := y1StartServer(t, dir, filepath.Join(dir, "served-runs"))
	start := y1RPC(t, base, "run.start", map[string]any{
		"runbookPath": y1Fixture(t, "native-values-served.runbook.yaml"),
	})
	runID := start["result"].(map[string]any)["runID"].(string)
	for {
		reply := y1RPC(t, base, "run.next", map[string]any{"runID": runID})
		if rpcError, ok := reply["error"].(map[string]any); ok {
			if rpcError["code"] == json.Number("-32011") || rpcError["code"] == float64(-32011) {
				break
			}
			t.Fatalf("run.next failed: %#v state=%#v", reply, y1RPC(t, base, "run.get", map[string]any{"runID": runID}))
		}
	}
	active := y1RPC(t, base, "run.get", map[string]any{"runID": runID})
	activeResult := active["result"].(map[string]any)
	var servedResults engine.RunResults
	body, _ := json.Marshal(activeResult["results"])
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&servedResults); err != nil {
		t.Fatal(err)
	}
	y1AssertNativeResults(t, &servedResults)
	stop()
	base, stop = y1StartServer(t, dir, filepath.Join(dir, "served-runs"))
	defer stop()
	persisted := y1RPC(t, base, "run.get", map[string]any{"runID": runID})
	persistedResult := persisted["result"].(map[string]any)
	if persistedResult["source"] != "persisted" || !reflect.DeepEqual(activeResult["results"], persistedResult["results"]) {
		t.Fatalf("served restart changed Results:\nactive=%#v\npersisted=%#v", activeResult, persistedResult)
	}
}

func TestY1RuntimeContractsChildForwardingAndBlockedNoData(t *testing.T) {
	dir := t.TempDir()
	successOut, successErr, err := y1RunCommand(t, y1Fixture(t, "parent-success.runbook.yaml"),
		"--run-dir", filepath.Join(dir, "success-runs"), "--output", "json")
	if err != nil {
		t.Fatalf("child success: %v\nstderr=%s\nstdout=%s", err, successErr, successOut)
	}
	var success y1DirectSummary
	decoder := json.NewDecoder(bytes.NewReader(successOut))
	decoder.UseNumber()
	if err := decoder.Decode(&success); err != nil {
		t.Fatal(err)
	}
	got := success.Results.Outputs["result"].Value
	want := map[string]any{"flag": false, "zero": json.Number("0"), "empty": "", "list": []any{}, "map": map[string]any{},
		"nested": map[string]any{"null": nil}, "unicode": "Niño 😀"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child publication was not forwarded exactly: %#v", got)
	}

	executable, _ := os.Executable()
	downstream := filepath.Join(dir, "downstream.txt")
	blockedOut, blockedErr, err := y1RunCommand(t, y1Fixture(t, "parent-blocked.runbook.yaml"),
		"--run-dir", filepath.Join(dir, "blocked-runs"), "--output", "json",
		"--var", "helper_path="+executable, "--var", "downstream_counter_path="+downstream)
	if err != nil {
		t.Fatalf("blocked run: %v\nstderr=%s\nstdout=%s", err, blockedErr, blockedOut)
	}
	var blocked y1DirectSummary
	if err := json.Unmarshal(blockedOut, &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Results != nil || blocked.ResultsUnavailable == nil || blocked.ResultsUnavailable.Reason() != "no-publication" {
		t.Fatalf("blocked child fabricated parent publication: %s", blockedOut)
	}
	if _, err := os.Stat(downstream); !os.IsNotExist(err) {
		t.Fatalf("blocked child dispatched downstream: %v", err)
	}
}

func TestY1RuntimeContractsUnsupportedCapabilityHasNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs")
	trace := filepath.Join(dir, "trace.jsonl")
	stdout, stderr, err := y1RunCommand(t, filepath.Join(dir, "must-not-be-read.yaml"),
		"--require-capabilities", "yawr.unsupported/v9", "--run-dir", runDir, "--trace", trace, "--output", "json")
	if err == nil || !bytes.Contains(stderr, []byte("unsupported-capability")) {
		t.Fatalf("negative control did not fail closed: err=%v stderr=%s stdout=%s", err, stderr, stdout)
	}
	for _, path := range []string{runDir, trace} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("unsupported capability created side effect %s: %v", path, statErr)
		}
	}
}

func y1StartServer(t *testing.T, workingDir, runDir string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	cmd := y1Command(t, "serve", "--addr", address, "--run-dir", runDir)
	cmd.Dir = workingDir
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if t.Failed() {
				t.Logf("serve output:\n%s", output.String())
			}
		})
	}
	t.Cleanup(stop)
	base := "http://" + address
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, base+"/health", nil)
		response, requestErr := (&http.Client{Timeout: time.Second}).Do(request)
		if requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return base, stop
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	stop()
	t.Fatalf("server did not become healthy: %s", output.String())
	return "", nil
}

func y1RPC(t *testing.T, base, method string, params map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/rpc", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var reply map[string]any
	if err := decoder.Decode(&reply); err != nil {
		t.Fatalf("decode RPC: %v body=%s", err, data)
	}
	return reply
}
