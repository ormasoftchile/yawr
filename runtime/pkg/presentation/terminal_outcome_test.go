package presentation

import (
	"context"
	"fmt"
	"testing"
)

func TestTerminalOutcomeExpressionSites(t *testing.T) {
	for _, publish := range []bool{false, true} {
		source := fmt.Sprintf("apiVersion: yawr.runbook/v1\nid: end\nname: End\nflow:\n  - step:\n      id: end\n      type: end\n      publish_results: %t\n      outcome:\n        category: '${child.status}'\n        code: '${child.code}'\n", publish)
		reply := ResolveExpressions(context.Background(), expressionRequest(t, source))
		want := 0
		if publish {
			want = 2
		}
		if len(reply.Regions) != want {
			t.Fatalf("publish=%v: expression regions=%d want=%d status=%s", publish, len(reply.Regions), want, reply.Status)
		}
	}
}
