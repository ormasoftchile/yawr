package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func TestStart_SubEventsCompleteBeforeParent(t *testing.T) {
	for _, count := range []int{1, 300} {
		t.Run(fmt.Sprintf("%d_children", count), func(t *testing.T) {
			var source strings.Builder
			source.WriteString(`apiVersion: yawr.runbook/v1
id: sub-events
name: Sub-event publication
vars:
  selected: first
flow:
  - step:
      id: choice
      type: branch
      branches:
        - condition: 'selected == "first"'
          label: selected arm
          steps:
`)
			for i := 0; i < count; i++ {
				fmt.Fprintf(&source, "            - step: {id: child_%d, type: noop}\n", i)
			}
			source.WriteString(`        - condition: 'selected != "first"'
          label: other arm
          steps:
            - step: {id: unselected, type: noop}
`)

			var mu sync.Mutex
			var observed []string
			var atParentCompletion []string
			parentCompleted := false
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			handle, err := Start(ctx, Config{
				RunbookPath: createTempRunbook(t, source.String()),
				Output:      io.Discard,
				OnSubEvent: func(event engine.Event) {
					if event.Kind == "step/started" || event.Kind == "step/completed" {
						mu.Lock()
						observed = append(observed, fmt.Sprintf("%v:%s", event.Payload["step_id"], event.Kind))
						mu.Unlock()
					}
				},
				OnEvent: func(event engine.Event) {
					if event.Kind == "step/completed" && event.Payload["step_id"] == "choice" {
						mu.Lock()
						atParentCompletion = append([]string(nil), observed...)
						parentCompleted = true
						mu.Unlock()
					}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			for {
				_, err = handle.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var want []string
			for i := 0; i < count; i++ {
				want = append(want, fmt.Sprintf("child_%d:step/started", i), fmt.Sprintf("child_%d:step/completed", i))
			}
			mu.Lock()
			defer mu.Unlock()
			if !parentCompleted {
				t.Fatal("parent completion was not observed")
			}
			if !slices.Equal(atParentCompletion, want) {
				t.Errorf("sub-events at parent completion: got %d, want %d in canonical order\n%v", len(atParentCompletion), len(want), atParentCompletion)
			}
			if !slices.Equal(observed, want) {
				t.Errorf("sub-events at EOF: got %d, want %d exactly once in canonical order\n%v", len(observed), len(want), observed)
			}
		})
	}
}
