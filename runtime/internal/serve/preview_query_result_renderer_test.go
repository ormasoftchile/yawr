package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/adapter"
	internalengine "github.com/ormasoftchile/yawr/runtime/internal/engine"
	internalparser "github.com/ormasoftchile/yawr/runtime/internal/parser"
	internalplanner "github.com/ormasoftchile/yawr/runtime/internal/planner"
	internaltool "github.com/ormasoftchile/yawr/runtime/internal/tool"
	"github.com/ormasoftchile/yawr/runtime/pkg/parser"
	plannerpkg "github.com/ormasoftchile/yawr/runtime/pkg/planner"
	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	servepkg "github.com/ormasoftchile/yawr/runtime/pkg/serve"
)

func TestPreviewQueryResultRenderer_ExecutesRealCardCode(t *testing.T) {
	texts := renderQueryResultTexts(t, map[string]any{
		"success":   true,
		"row_count": 12,
		"columns":   []any{"Column1", "Unsafe"},
		"rows": func() []any {
			rows := make([]any, 12)
			for i := range rows {
				unsafe := "safe"
				if i == 0 {
					unsafe = "<img src=x onerror=alert(1)>"
				}
				rows[i] = map[string]any{"Column1": i + 1, "Unsafe": unsafe}
			}
			return rows
		}(),
		"stdout": `{"success":true}`,
	})
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"Query succeeded", "Rows: 12", "Columns: Column1, Unsafe", "Sample (10 of 12)", "Showing 10 of 12 rows", "Raw stdout", "<img src=x onerror=alert(1)>"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rendered card missing %q in texts:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dangerouslySetInnerHTML") {
		t.Fatalf("renderer exposed raw HTML rendering escape hatch")
	}
}

func TestPreviewQueryResultRenderer_EnforcesTotalBudgetForColumnsAndFirstRow(t *testing.T) {
	columns := make([]any, 101)
	row := make(map[string]any, 101)
	for i := range columns {
		name := strings.Repeat("LongColumn", 40) + string(rune('A'+(i%26)))
		columns[i] = name
		row[name] = strings.Repeat("cell-value-", 400)
	}
	texts := renderQueryResultTexts(t, map[string]any{
		"success":   true,
		"row_count": 1,
		"columns":   columns,
		"rows":      []any{row},
		"stdout":    strings.Repeat("raw-stdout", 20000),
	})
	joined := strings.Join(texts, "")
	if len(joined) > 12000 {
		t.Fatalf("rendered query card exceeded declared budget: got %d chars, want <= 12000", len(joined))
	}
	visible := strings.Join(texts, "\n")
	for _, want := range []string{"Showing 1 of 1 rows and", "Display budget reached"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("budgeted renderer did not honestly report truncation %q in:\n%s", want, visible)
		}
	}
}

func TestPreviewQueryResultRenderer_EmptyAndErrorStates(t *testing.T) {
	emptyTexts := strings.Join(renderQueryResultTexts(t, map[string]any{
		"success":   true,
		"row_count": 0,
		"columns":   []any{"Column1"},
		"rows":      []any{},
		"stdout":    `{"success":true,"rowCount":0,"columns":["Column1"],"data":[]}`,
	}), "\n")
	if !strings.Contains(emptyTexts, "Query succeeded") || !strings.Contains(emptyTexts, "0 rows") {
		t.Fatalf("empty success not rendered as 0-row success:\n%s", emptyTexts)
	}
	if strings.Contains(emptyTexts, "Query result error") {
		t.Fatalf("empty success rendered as structured error:\n%s", emptyTexts)
	}

	errorTexts := strings.Join(renderQueryResultErrorTexts(t, "native tool queryer action query: result: stdout must be exactly one JSON object", map[string]any{"stdout": "not-json"}), "\n")
	if !strings.Contains(errorTexts, "Query result error") || !strings.Contains(errorTexts, "Raw stdout") {
		t.Fatalf("structured error state not distinct/diagnostic:\n%s", errorTexts)
	}
}

func TestPreviewQueryResultNativeHelperProcess(t *testing.T) {
	mode := os.Getenv("YAWR_PREVIEW_QUERY_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprintln(os.Stderr, "diagnostic")
	if mode == "large" {
		columns := make([]string, 16)
		rows := make([]map[string]string, 400)
		for i := range columns {
			columns[i] = fmt.Sprintf("Column%02d_%s", i, strings.Repeat("header", 4))
		}
		for i := range rows {
			row := make(map[string]string, len(columns))
			for _, col := range columns {
				row[col] = strings.Repeat(fmt.Sprintf("row-%03d-", i), 8)
			}
			rows[i] = row
		}
		payload, _ := json.Marshal(map[string]any{"success": true, "rowCount": 400, "columns": columns, "data": rows, "metadata": map[string]any{"source": strings.Repeat("m", 5000)}})
		fmt.Print(string(payload))
		os.Exit(0)
	}
	fmt.Print(`{"success":true,"rowCount":1,"columns":["Column1"],"data":[{"Column1":1}],"metadata":{"source":"helper"}}`)
	os.Exit(0)
}

func TestE2E_ServedPreviewQueryResultCardPopulatedFromRealNativeRun(t *testing.T) {
	ts, runbookPath := startPreviewQueryServer(t, "1")
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runs", "application/json", strings.NewReader(`{"runbookPath":`+quoteJSON(runbookPath)+`}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST /runs status: got %d want 201; body=%s", resp.StatusCode, raw)
	}
	var createResp struct {
		RunID string `json:"runID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		resp.Body.Close()
		t.Fatalf("decode POST /runs response: %v", err)
	}
	resp.Body.Close()
	if createResp.RunID == "" {
		t.Fatal("POST /runs returned empty runID")
	}

	output := waitForQueryOutputFromServedState(t, ts.URL, createResp.RunID)
	texts := renderQueryResultTexts(t, output)
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"Query succeeded", "Rows: 1", "Columns: Column1", "Sample (1 of 1)", "Raw stdout excerpt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("served preview query card missing %q in texts:\n%s", want, joined)
		}
	}
	if _, exists := output["stdout"]; exists {
		t.Fatalf("served preview state carried full stdout instead of bounded excerpt: %#v", output)
	}
}

func TestE2E_ServedPreviewQueryResultStateAndCardAreBoundedForHugeNativeRun(t *testing.T) {
	ts, runbookPath := startPreviewQueryServer(t, "large")
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runs", "application/json", strings.NewReader(`{"runbookPath":`+quoteJSON(runbookPath)+`}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST /runs status: got %d want 201; body=%s", resp.StatusCode, raw)
	}
	var createResp struct {
		RunID string `json:"runID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		resp.Body.Close()
		t.Fatalf("decode POST /runs response: %v", err)
	}
	resp.Body.Close()

	output, frameSize := waitForQueryOutputFrameFromServedState(t, ts.URL, createResp.RunID)
	if frameSize > 30000 {
		t.Fatalf("served state frame is unbounded: got %d bytes, want <= 30000", frameSize)
	}
	if _, exists := output["stdout"]; exists {
		t.Fatalf("served preview state carried full stdout instead of excerpt")
	}
	if output["stdout_truncated"] != true || output["preview_truncated"] != true {
		t.Fatalf("expected truncation metadata in bounded output: %#v", output)
	}
	if got := len(fmt.Sprint(output["stdout_excerpt"])); got > 4096 {
		t.Fatalf("stdout excerpt too large: got %d", got)
	}
	texts := renderQueryResultTexts(t, output)
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"Rows: 400", "Preview output truncated", "Raw stdout excerpt (truncated)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("bounded served preview missing %q in texts:\n%s", want, joined)
		}
	}
}

func TestE2E_ServedPreviewQueryResultCapturedRowsAreBounded(t *testing.T) {
	ts, runbookPath := startPreviewQueryServer(t, "large", true, true)
	defer ts.Close()

	eventsResp, err := http.Get(ts.URL + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(eventsResp.Body)
		t.Fatalf("GET /events status: got %d want 200; body=%s", eventsResp.StatusCode, raw)
	}

	resp, err := http.Post(ts.URL+"/runs", "application/json", strings.NewReader(`{"runbookPath":`+quoteJSON(runbookPath)+`}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST /runs status: got %d want 201; body=%s", resp.StatusCode, raw)
	}
	var createResp struct {
		RunID string `json:"runID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		resp.Body.Close()
		t.Fatalf("decode POST /runs response: %v", err)
	}
	resp.Body.Close()

	eventFrame := waitForStepCompletedEventFrame(t, eventsResp.Body, createResp.RunID)
	if got := len(eventFrame.data); got > 30000 {
		t.Fatalf("/events step/completed payload is unbounded: got %d bytes, want <= 30000", got)
	}
	var event struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(eventFrame.data), &event); err != nil {
		t.Fatalf("decode /events frame: %v\n%s", err, eventFrame.data)
	}
	captures, _ := event.Payload["captures"].(map[string]any)
	if captures["saved_rows_preview_truncated"] != true {
		t.Fatalf("/events captures missing truncation marker: %#v", captures)
	}
	if jsonSizeForTest(captures["saved_rows"]) > 4096 {
		t.Fatalf("/events captured rows exceed preview budget")
	}

	output, frameSize := waitForQueryOutputFrameFromServedState(t, ts.URL, createResp.RunID)
	_ = output
	if frameSize > 30000 {
		t.Fatalf("/state frame with captured rows is unbounded: got %d bytes, want <= 30000", frameSize)
	}
	stateFrame := readLatestStateFrame(t, ts.URL, createResp.RunID)
	if !strings.Contains(stateFrame, "saved_rows_preview_truncated") {
		t.Fatalf("/state frame missing captured rows truncation marker: %s", stateFrame)
	}
	if strings.Contains(stateFrame, strings.Repeat("row-000-", 80)) {
		t.Fatalf("/state frame contains unsampled captured row data")
	}
}

func TestE2E_ServedPreviewQueryResultSynthesizedCapturesPlanAndExecute(t *testing.T) {
	ts, runbookPath := startPreviewQueryServer(t, "1", true)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runs", "application/json", strings.NewReader(`{"runbookPath":`+quoteJSON(runbookPath)+`}`))
	if err != nil {
		t.Fatalf("POST /runs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST /runs status: got %d want 201; body=%s", resp.StatusCode, raw)
	}
	var createResp struct {
		RunID string `json:"runID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		resp.Body.Close()
		t.Fatalf("decode POST /runs response: %v", err)
	}
	resp.Body.Close()

	stateFrame := waitForStateFrameContaining(t, ts.URL, createResp.RunID, "saved_count")
	if !strings.Contains(stateFrame, `"saved_count":1`) || !strings.Contains(stateFrame, "saved_rows") {
		t.Fatalf("synthesized captures did not execute into preview state: %s", stateFrame)
	}
}

func startPreviewQueryServer(t *testing.T, helperMode string, captureRows ...bool) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	toolPath := filepath.Join(root, "queryer.tool.yaml")
	runbookPath := filepath.Join(root, "query-runbook.yaml")
	captureBlock := ""
	if len(captureRows) > 0 && captureRows[0] {
		captureBlock = "      capture:\n        saved_count: outputs.row_count\n        saved_rows: outputs.rows\n"
	}
	outputsBlock := ""
	if len(captureRows) > 1 && captureRows[1] {
		outputsBlock = "    outputs:\n      row_count: {type: integer}\n      rows: {type: array}\n"
	}
	if err := os.WriteFile(toolPath, []byte(`apiVersion: yawr.tool/v1
meta:
  name: queryer
  version: "1.0.0"
transport:
  mode: native
  command: '`+yamlSingleQuoted(os.Args[0])+`'
  env:
    YAWR_PREVIEW_QUERY_HELPER: "`+helperMode+`"
actions:
  - name: query
    argv: ["-test.run=TestPreviewQueryResultNativeHelperProcess", "--"]
`+outputsBlock+`    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata
`), 0o644); err != nil {
		t.Fatalf("write tool fixture: %v", err)
	}
	if err := os.WriteFile(runbookPath, []byte(`apiVersion: yawr.runbook/v1
id: query-preview
name: Query Preview
flow:
  - step:
      id: query
      type: tool
      title: Run local query
      tool:
        name: queryer
        action: query
`+captureBlock), 0o644); err != nil {
		t.Fatalf("write runbook fixture: %v", err)
	}

	plat := platform.Real()
	parserImpl, err := internalparser.New(plat)
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	schemaDef, err := internaltool.ParseToolFile(toolPath)
	if err != nil {
		t.Fatalf("parse tool fixture: %v", err)
	}
	plannerImpl := internalplanner.New(plannerpkg.Config{
		Loader: &previewQueryFileLoader{parser: parserImpl},
		Tools:  previewQueryToolRegistry{defs: map[string]*schema.ToolDef{"queryer": schemaDef}},
	})
	runDir := filepath.Join(root, "runs")
	traceDir := filepath.Join(root, "traces")
	engineCfg, shutdown, err := adapter.BuildEngineConfig(context.Background(), adapter.WireOptions{
		RunDir:             runDir,
		TraceDir:           traceDir,
		ToolScanDir:        root,
		ExcludeTestTools:   false,
		TTYOutput:          false,
		LazyRunbookLoader:  adapter.NewParserLazyLoader(parserImpl),
		SubstitutionParser: parserImpl,
	})
	if err != nil {
		t.Fatalf("BuildEngineConfig: %v", err)
	}
	t.Cleanup(shutdown)
	server, err := NewServer(servepkg.ServerConfig{
		Engine:       internalengine.New(engineCfg),
		EngineConfig: engineCfg,
		Parser:       parserImpl,
		Planner:      plannerImpl,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Stop(stopCtx); err != nil {
			t.Errorf("Stop server: %v", err)
		}
	})
	return newHTTPTestServer(t, server), runbookPath
}

func waitForStepCompletedEventFrame(t *testing.T, body io.Reader, runID string) sseFrame {
	t.Helper()
	br := bufio.NewReader(body)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := readRawSSEFrame(br, 30*time.Second)
		if err != nil {
			t.Fatalf("read /events frame: %v", err)
		}
		if frame.event != "step/completed" {
			continue
		}
		var ev struct {
			RunID string `json:"runID"`
		}
		if err := json.Unmarshal([]byte(frame.data), &ev); err != nil {
			t.Fatalf("decode /events runID: %v\n%s", err, frame.data)
		}
		if ev.RunID == runID {
			return frame
		}
	}
	t.Fatal("timed out waiting for /events step/completed frame")
	return sseFrame{}
}

func waitForStateFrameContaining(t *testing.T, baseURL, runID, needle string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		frame := readLatestStateFrame(t, baseURL, runID)
		last = frame
		if strings.Contains(frame, needle) {
			return frame
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("state frame never contained %q; last=%s", needle, last)
	return ""
}

func readLatestStateFrame(t *testing.T, baseURL, runID string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/runs/" + runID + "/state")
	if err != nil {
		t.Fatalf("GET state: %v", err)
	}
	defer resp.Body.Close()
	frame, err := readRawSSEFrame(bufio.NewReader(resp.Body), time.Second)
	if err != nil {
		t.Fatalf("read state frame: %v", err)
	}
	return frame.data
}

func jsonSizeForTest(value any) int {
	b, err := json.Marshal(value)
	if err != nil {
		return len(fmt.Sprint(value))
	}
	return len(b)
}

func waitForQueryOutputFromServedState(t *testing.T, baseURL, runID string) map[string]any {
	t.Helper()
	output, _ := waitForQueryOutputFrameFromServedState(t, baseURL, runID)
	return output
}

func waitForQueryOutputFrameFromServedState(t *testing.T, baseURL, runID string) (map[string]any, int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastFrame string
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/runs/" + runID + "/state")
		if err != nil {
			t.Fatalf("GET state: %v", err)
		}
		frame, readErr := readRawSSEFrame(bufio.NewReader(resp.Body), time.Second)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read state frame: %v", readErr)
		}
		var state struct {
			Nodes map[string]struct {
				Output map[string]any `json:"output"`
			} `json:"nodes"`
		}
		lastFrame = frame.data
		if err := json.Unmarshal([]byte(frame.data), &state); err != nil {
			t.Fatalf("decode state frame: %v\n%s", err, frame.data)
		}
		if state.Nodes["query"].Output != nil {
			return state.Nodes["query"].Output, len(frame.data)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("served run state never exposed query step output; last state frame: %s", lastFrame)
	return nil, 0
}

type previewQueryFileLoader struct{ parser parser.Parser }

func (l *previewQueryFileLoader) Load(ctx context.Context, path string) (*parser.ParsedRunbook, error) {
	return l.parser.Parse(ctx, path)
}

type previewQueryToolRegistry struct{ defs map[string]*schema.ToolDef }

func (r previewQueryToolRegistry) Lookup(_ context.Context, name string, action string) (*schema.ToolDef, error) {
	def := r.defs[name]
	if def == nil {
		return nil, plannerpkg.ErrToolNotFound
	}
	if def.Actions[action] == nil {
		return nil, plannerpkg.ErrActionNotFound
	}
	return def, nil
}

func yamlSingleQuoted(s string) string { return strings.ReplaceAll(s, "'", "''") }

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func renderQueryResultTexts(t *testing.T, output map[string]any) []string {
	t.Helper()
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	html := string(data)
	snippets := []string{
		extractConst(t, html, "QUERY_RESULT_SAMPLE_ROWS"),
		extractConst(t, html, "QUERY_RESULT_CELL_CHAR_LIMIT"),
		extractConst(t, html, "QUERY_RESULT_TOTAL_CHAR_BUDGET"),
		extractConstOrDefault(html, "QUERY_RESULT_UI_TEXT_RESERVE", "0"),
		extractConstOrDefault(html, "QUERY_RESULT_DIAGNOSTIC_CHAR_BUDGET", "4096"),
		extractJSFunction(t, html, "isQueryResultOutput"),
		extractJSFunction(t, html, "QueryResultErrorCard"),
		extractJSFunction(t, html, "QueryResultCard"),
		extractJSFunction(t, html, "boundedQuerySample"),
		extractJSFunction(t, html, "queryCellValue"),
		extractJSFunction(t, html, "formatQueryCell"),
		extractJSFunctionOrDefault(html, "truncateToBudget", "function truncateToBudget(text, budget) { if (budget <= 0) return ''; if (text.length <= budget) return text; if (budget === 1) return '\\u2026'; return text.slice(0, budget - 1) + '\\u2026'; }"),
		extractJSFunctionOrDefault(html, "fitCellsToBudget", "function fitCellsToBudget(cells, budget) { return cells; }"),
		extractJSFunctionOrDefault(html, "queryDiagnosticExcerpt", "function queryDiagnosticExcerpt(output, name) { return { text: output && output[name] ? String(output[name]) : '', truncated: false }; }"),
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal renderer output fixture: %v", err)
	}
	script := `
const e = (type, props, ...children) => ({ type, props: props || {}, children });
` + strings.Join(snippets, "\n") + `
const output = ` + string(encodedOutput) + `;
if (!isQueryResultOutput(output)) throw new Error('query result not recognized');
const tree = QueryResultCard({ output });
const texts = [];
function walk(node) {
  if (node === null || node === undefined || node === false) return;
  if (typeof node === 'string' || typeof node === 'number' || typeof node === 'boolean') { texts.push(String(node)); return; }
  if (Array.isArray(node)) { node.forEach(walk); return; }
  if (node.children) node.children.forEach(walk);
}
walk(tree);
console.log(JSON.stringify({texts, tree}));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node renderer harness failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var got struct {
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode renderer harness output: %v\n%s", err, out)
	}
	return got.Texts
}

func renderQueryResultErrorTexts(t *testing.T, errText string, output map[string]any) []string {
	t.Helper()
	data, err := staticFS.ReadFile("static/preview.html")
	if err != nil {
		t.Fatalf("read embedded preview.html: %v", err)
	}
	html := string(data)
	snippets := []string{
		extractConstOrDefault(html, "QUERY_RESULT_DIAGNOSTIC_CHAR_BUDGET", "4096"),
		extractJSFunctionOrDefault(html, "truncateToBudget", "function truncateToBudget(text, budget) { if (budget <= 0) return ''; if (text.length <= budget) return text; if (budget === 1) return '\\u2026'; return text.slice(0, budget - 1) + '\\u2026'; }"),
		extractJSFunction(t, html, "queryDiagnosticExcerpt"),
		extractJSFunction(t, html, "QueryResultErrorCard"),
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal renderer output fixture: %v", err)
	}
	encodedErr, err := json.Marshal(errText)
	if err != nil {
		t.Fatalf("marshal renderer error fixture: %v", err)
	}
	script := `
const e = (type, props, ...children) => ({ type, props: props || {}, children });
` + strings.Join(snippets, "\n") + `
const tree = QueryResultErrorCard({ error: ` + string(encodedErr) + `, output: ` + string(encodedOutput) + ` });
const texts = [];
function walk(node) {
  if (node === null || node === undefined || node === false) return;
  if (typeof node === 'string' || typeof node === 'number' || typeof node === 'boolean') { texts.push(String(node)); return; }
  if (Array.isArray(node)) { node.forEach(walk); return; }
  if (node.children) node.children.forEach(walk);
}
walk(tree);
console.log(JSON.stringify({texts, tree}));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node error renderer harness failed: %v\nstderr:\n%s", err, stderr.String())
	}
	var got struct {
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode error renderer harness output: %v\n%s", err, out)
	}
	return got.Texts
}

func extractConstOrDefault(src, name, value string) string {
	prefix := "const " + name
	if strings.Contains(src, prefix) {
		start := strings.Index(src, prefix)
		end := strings.Index(src[start:], ";")
		if end >= 0 {
			return src[start : start+end+1]
		}
	}
	return "const " + name + " = " + value + ";"
}

func extractConst(t *testing.T, src, name string) string {
	t.Helper()
	prefix := "const " + name
	start := strings.Index(src, prefix)
	if start < 0 {
		t.Fatalf("%s not found", name)
	}
	end := strings.Index(src[start:], ";")
	if end < 0 {
		t.Fatalf("%s missing semicolon", name)
	}
	return src[start : start+end+1]
}

func extractJSFunctionOrDefault(src, name, fallback string) string {
	if strings.Contains(src, "function "+name+"(") {
		start := strings.Index(src, "function "+name+"(")
		bodyOpen := strings.Index(src[start:], ") {")
		if bodyOpen >= 0 {
			idx := start + bodyOpen + len(") ")
			depth := 0
			inString := byte(0)
			escaped := false
			for i := idx; i < len(src); i++ {
				ch := src[i]
				if inString != 0 {
					if escaped {
						escaped = false
						continue
					}
					if ch == '\\' {
						escaped = true
						continue
					}
					if ch == inString {
						inString = 0
					}
					continue
				}
				switch ch {
				case '\'', '"', '`':
					inString = ch
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						return src[start : i+1]
					}
				}
			}
		}
	}
	return fallback
}

func extractJSFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	bodyOpen := strings.Index(src[start:], ") {")
	if bodyOpen < 0 {
		t.Fatalf("function %s missing body", name)
	}
	idx := start + bodyOpen + len(") ")
	depth := 0
	inString := byte(0)
	escaped := false
	for i := idx; i < len(src); i++ {
		ch := src[i]
		if inString != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == inString {
				inString = 0
			}
			continue
		}
		switch ch {
		case '\'', '"', '`':
			inString = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("function %s body did not close", name)
	return ""
}
