package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/internal/resultsdelivery"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func y2Fixture(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "y2-contract-gap-closure", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func y2DecodeUnavailable(t *testing.T, raw []byte, wantReason string) resultsdelivery.Unavailable {
	t.Helper()
	var unavailable resultsdelivery.Unavailable
	if err := json.Unmarshal(raw, &unavailable); err != nil {
		t.Fatal(err)
	}
	if unavailable.Reason() != wantReason {
		t.Fatalf("reason=%q, want %q", unavailable.Reason(), wantReason)
	}
	return unavailable
}

func TestY2ResultsUnavailableDirectResumeServedAndReopened(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	downstream := filepath.Join(dir, "must-not-run.txt")
	stdout, stderr, err := y1RunCommand(t, y1Fixture(t, "parent-blocked.runbook.yaml"), "--run-dir", runDir, "--output", "json",
		"--var", "helper_path="+executable, "--var", "downstream_counter_path="+downstream)
	if err != nil {
		t.Fatalf("direct: %v\nstderr=%s\nstdout=%s", err, stderr, stdout)
	}
	var direct struct {
		RunID              string          `json:"run_id"`
		Status             string          `json:"status"`
		Results            json.RawMessage `json:"results"`
		ResultsUnavailable json.RawMessage `json:"results_unavailable"`
	}
	if err := json.Unmarshal(stdout, &direct); err != nil {
		t.Fatal(err)
	}
	if direct.Status != "completed" || string(direct.Results) != "null" {
		t.Fatalf("direct execution status/results changed: %s", stdout)
	}
	y2DecodeUnavailable(t, direct.ResultsUnavailable, "no-publication")
	if err := filepath.WalkDir(runDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("results_unavailable")) || bytes.Contains(data, []byte("no-publication")) {
			t.Fatalf("delivery envelope persisted in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	base, stop := y1StartServer(t, dir, runDir)
	persisted := y1RPC(t, base, "run.get", map[string]any{"runID": direct.RunID})
	persistedResult := persisted["result"].(map[string]any)
	if persistedResult["source"] != "persisted" || persistedResult["state"] != "completed" || persistedResult["results"] != nil {
		t.Fatalf("persisted unavailable response changed: %#v", persistedResult)
	}
	raw, _ := json.Marshal(persistedResult["results_unavailable"])
	y2DecodeUnavailable(t, raw, "no-publication")
	stop()

	activeRunDir := filepath.Join(dir, "active-runs")
	base, stop = y1StartServer(t, dir, activeRunDir)
	defer stop()
	start := y1RPC(t, base, "run.start", map[string]any{"runbookPath": y2Fixture(t, "no-publication.runbook.yaml")})
	runID := start["result"].(map[string]any)["runID"].(string)
	for {
		reply := y1RPC(t, base, "run.next", map[string]any{"runID": runID})
		if _, ok := reply["error"]; ok {
			break
		}
	}
	active := y1RPC(t, base, "run.get", map[string]any{"runID": runID})["result"].(map[string]any)
	if active["source"] != "active" || active["state"] != "completed" || active["results"] != nil {
		t.Fatalf("active unavailable response changed: %#v", active)
	}
	raw, _ = json.Marshal(active["results_unavailable"])
	y2DecodeUnavailable(t, raw, "no-publication")
}

func TestY2ResultsOpenStdinInlineUnavailableAndChunked(t *testing.T) {
	runStdio := func(t *testing.T, fixture string, args ...string) []map[string]json.RawMessage {
		t.Helper()
		commandArgs := append([]string{y2Fixture(t, fixture), "--stdio", "--run-dir", filepath.Join(t.TempDir(), "runs")}, args...)
		cmd := y1Command(t, "run", commandArgs...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("stdio: %v\nstderr=%s\nstdout=%s", err, stderr.String(), stdout.String())
		}
		var frames []map[string]json.RawMessage
		scanner := bufio.NewScanner(&stdout)
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			var frame map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				t.Fatal(err)
			}
			frames = append(frames, frame)
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		return frames
	}

	unavailableFrames := runStdio(t, "no-publication.runbook.yaml")
	terminal := unavailableFrames[len(unavailableFrames)-1]
	if string(terminal["type"]) != `"run.finished"` ||
		string(terminal["status"]) != `"completed"` || string(terminal["results"]) != "null" {
		t.Fatalf("invalid unavailable terminal: %#v", terminal)
	}
	y2DecodeUnavailable(t, terminal["results_unavailable"], "no-publication")

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	frames := runStdio(t, "chunked-results.runbook.yaml", "--var", "helper_path="+executable)
	terminal = frames[len(frames)-1]
	if len(frames) < 3 || string(terminal["type"]) != `"run.finished"` || len(terminal["results"]) != 0 {
		t.Fatalf("chunk transport was not used: %d frames", len(frames))
	}
	var assembled []byte
	for _, frame := range frames[:len(frames)-1] {
		if string(frame["type"]) != `"run.results.chunk"` {
			continue
		}
		var offset, total int
		var data string
		if err := json.Unmarshal(frame["offset"], &offset); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(frame["totalBytes"], &total); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(frame["data"], &data); err != nil {
			t.Fatal(err)
		}
		part, err := base64.StdEncoding.DecodeString(data)
		if err != nil || offset != len(assembled) || len(part) > resultsdelivery.ChunkBytes {
			t.Fatal("invalid production chunk")
		}
		assembled = append(assembled, part...)
		if len(assembled) > total {
			t.Fatal("chunk bytes exceed declared total")
		}
	}
	var reference resultsdelivery.Reference
	if err := json.Unmarshal(terminal["results_ref"], &reference); err != nil {
		t.Fatal(err)
	}
	if reference.TotalBytes != len(assembled) {
		t.Fatal("chunk reference byte count mismatch")
	}
	var record engine.RunResults
	decoder := json.NewDecoder(bytes.NewReader(assembled))
	decoder.UseNumber()
	if err := decoder.Decode(&record); err != nil || record.Validate() != nil || record.Digest != reference.Digest {
		t.Fatalf("invalid assembled production Results: %v", err)
	}
	if got, ok := record.Outputs["result"].Value.(string); !ok || len(got) < 1<<20 {
		t.Fatal("chunk fixture did not cross inline limit")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("trailing assembled JSON", err)
	}
	if record.Outputs["result"].Type != "string" {
		t.Fatal("chunked result type changed")
	}
}
