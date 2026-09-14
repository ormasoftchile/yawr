package graphdoc_test

import (
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
)

func TestTerminalOutcomeGraphExpressions(t *testing.T) {
	for _, publish := range []bool{false, true} {
		details := &graphdoc.StepDetails{Kind: "end", PublishResults: publish, Category: "${child.status}", Code: "${child.code}"}
		details.ProjectExpressions()
		if !publish {
			if details.ExpressionPresentation != nil {
				t.Fatal("legacy literal outcome acquired expression metadata")
			}
			continue
		}
		if details.ExpressionPresentation == nil || len(details.ExpressionPresentation.Values) != 2 {
			t.Fatal("publishing end lost outcome expression metadata")
		}
		if details.ExpressionPresentation.Values[0].Path != "/category" || details.ExpressionPresentation.Values[1].Path != "/code" {
			t.Fatal("outcome expression projection has wrong paths")
		}
	}
}
