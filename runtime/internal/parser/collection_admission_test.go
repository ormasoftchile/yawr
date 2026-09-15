package parser

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ormasoftchile/yawr/runtime/pkg/platform"
)

func TestCollectionValuesSyntaxIsAdmittedBeforePlanning(t *testing.T) {
	parser, err := New(platform.NewFakePlatform())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{`'${rows}'`, true},
		{`{empty: [], nested: {rows: '${rows}'}, present: false}`, true},
		{`'${rows +}'`, false},
		{`{nested: {rows: '${rows +}'}}`, false},
		{`[1, {rows: '${rows +}'}]`, false},
		{`'${list.order(items, "left +")}'`, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			source := fmt.Sprintf(`apiVersion: yawr.runbook/v1
id: collection-admission
name: Collection admission
flow:
  - iterate:
      id: gather
      over: items
      steps:
        - step: {id: child, type: noop}
      collect_values:
        records: %s
`, test.value)
			_, err := parser.ParseBytes(context.Background(), []byte(source))
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && (err == nil || !strings.Contains(err.Error(), "collect_values.records")) {
				t.Fatalf("invalid collection syntax was not rejected at admission: %v", err)
			}
		})
	}
}
