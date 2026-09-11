package tool

import (
	"strings"
	"testing"
)

const resultContractToolHeader = `apiVersion: yawr.tool/v1
meta:
  name: queryer
  version: "1.0.0"
transport:
  mode: native
  command: queryer
actions:
`

func parseResultContractTool(t *testing.T, actionYAML string) error {
	t.Helper()
	_, err := parseToolFileFromString(t, resultContractToolHeader+actionYAML)
	return err
}

func TestResultContract_QueryResultValid(t *testing.T) {
	yaml := `  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata
`
	def, err := parseToolFileFromString(t, resultContractToolHeader+yaml)
	if err != nil {
		t.Fatalf("valid result contract rejected: %v", err)
	}
	runtimeDef, err := RuntimeToolDef(def)
	if err != nil {
		t.Fatalf("RuntimeToolDef: %v", err)
	}
	for _, name := range []string{"success", "row_count", "columns", "rows", "metadata"} {
		if runtimeDef.Actions["query"].Outputs[name] == nil {
			t.Fatalf("query-result semantic output %q was not synthesized", name)
		}
	}
}

func TestResultContract_QueryResultExactOutputDeclarationsValid(t *testing.T) {
	yaml := `  - name: query
    argv: ["query"]
    outputs:
      success: {type: boolean}
      row_count: {type: integer}
      columns: {type: array}
      rows: {type: array}
      metadata: {type: object, optional: true}
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata
`
	if err := parseResultContractTool(t, yaml); err != nil {
		t.Fatalf("exact query-result output declarations rejected: %v", err)
	}
}

func TestResultContract_RejectsSemanticOutputCollisions(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "row_count string collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      row_count: {type: string}
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.row_count conflicts",
		},
		{
			name: "success string collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      success: {type: string}
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.success conflicts",
		},
		{
			name: "metadata nonoptional collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      metadata: {type: object}
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata
`,
			wantErr: "outputs.metadata conflicts",
		},
		{
			name: "metadata declared without mapping",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      metadata: {type: object, optional: true}
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.metadata conflicts",
		},
		{
			name: "success null collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      success: null
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.success conflicts",
		},
		{
			name: "row_count null collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      row_count: null
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.row_count conflicts",
		},
		{
			name: "columns null collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      columns: null
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.columns conflicts",
		},
		{
			name: "rows null collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      rows: null
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "outputs.rows conflicts",
		},
		{
			name: "metadata null collision",
			yaml: `  - name: query
    argv: ["query"]
    outputs:
      metadata: null
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      metadata: metadata
`,
			wantErr: "outputs.metadata conflicts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseResultContractTool(t, tc.yaml)
			if err == nil {
				t.Fatal("expected parse error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestResultContract_RejectsUnknownFormatSourceAndMappings(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unknown format",
			yaml: `  - name: query
    argv: ["query"]
    result:
      format: json
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "result.format",
		},
		{
			name: "unknown source",
			yaml: `  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stderr-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "result.source",
		},
		{
			name: "missing row count mapping",
			yaml: `  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      columns: columns
      rows: data
`,
			wantErr: "result.row_count",
		},
		{
			name: "nested mapping rejected",
			yaml: `  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: stats.rowCount
      columns: columns
      rows: data
`,
			wantErr: "result.row_count",
		},
		{
			name: "unknown result key rejected",
			yaml: `  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
      samples: sampleRows
`,
			wantErr: "result.samples",
		},
		{
			name: "result on non-native transport",
			yaml: `apiVersion: yawr.tool/v1
meta:
  name: queryer
  version: "1.0.0"
transport:
  mode: stdio
actions:
  - name: query
    argv: ["query"]
    result:
      format: yawr.query-result/v1
      source: stdout-json
      row_count: rowCount
      columns: columns
      rows: data
`,
			wantErr: "only supported for native",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			if strings.HasPrefix(tc.yaml, "apiVersion:") {
				_, err = parseToolFileFromString(t, tc.yaml)
			} else {
				err = parseResultContractTool(t, tc.yaml)
			}
			if err == nil {
				t.Fatal("expected parse error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
