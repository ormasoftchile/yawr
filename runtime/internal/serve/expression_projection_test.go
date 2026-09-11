package serve

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/graphdoc"
	"github.com/ormasoftchile/yawr/runtime/pkg/preview/render/graphjson"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
)

func TestExpressionDocumentTransportAndSanitization(t *testing.T) {
	details := graphdoc.DetailsForResolvedStep(engine.ResolvedStep{Kind: "display", When: "count >= 2", Spec: &schema.DisplaySpec{Display: schema.DisplayConfig{Content: "Hi ${name}!"}}})
	doc := &graphdoc.Document{Nodes: []graphdoc.Node{{ID: "message", Kind: "display", Details: details}}}
	doc.Hash, _ = doc.ContentHash()
	priorETag := ""
	for _, redacted := range []bool{false, true} {
		if redacted {
			details.Content = "<redacted>"
		}
		w := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/preview/document", nil)
		request.Header.Set("If-None-Match", priorETag)
		writeDocument(w, request, doc, "graphjson")
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var wire graphjson.Document
		if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(wire.Nodes[0].Data["details"])
		var got graphdoc.StepDetails
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		want := 2
		if redacted {
			want = 1
		}
		if got.ExpressionPresentation == nil || len(got.ExpressionPresentation.Values) != want {
			t.Fatal(got.ExpressionPresentation)
		}
		if redacted && got.ExpressionPresentation.Values[0].Path != "/common/when" {
			t.Fatal("tokens survived redaction")
		}
		if redacted && priorETag == w.Header().Get("ETag") {
			t.Fatal("sanitized graph reused stale ETag")
		}
		priorETag = w.Header().Get("ETag")
	}
}
