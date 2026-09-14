package parser

import (
	"context"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func TestParserTerminalResultsConcurrency(t *testing.T) {
	parser, err := New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	prefix := "apiVersion: yawr.runbook/v1\nid: terminal\nname: Terminal\nflow:\n"
	sequential := `  - iterate:
      id: loop
      over: items
      concurrency: 1
      steps:
        - step: {id: end, type: end, publish_results: true}
`
	if _, err := parser.ParseBytes(context.Background(), []byte(prefix+sequential)); err != nil {
		t.Fatal(err)
	}
	for _, flow := range []string{
		strings.Replace(sequential, "concurrency: 1", "concurrency: 2", 1),
		`  - parallel:
      id: concurrent
      branches:
        - label: first
          steps:
            - step: {id: end, type: end, publish_results: true}
`,
	} {
		if _, err := parser.ParseBytes(context.Background(), []byte(prefix+flow)); err == nil || !strings.Contains(err.Error(), "cannot publish terminal results") {
			t.Fatalf("concurrent terminal publication not rejected: %v", err)
		}
	}
}
