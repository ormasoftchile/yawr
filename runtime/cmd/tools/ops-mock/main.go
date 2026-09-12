// Package main is the deterministic native mock binary for the ops-synthetic
// contract proof. It implements two logical tool actions:
//
//   - status-query: returns a 5-field typed output (Action A, mirrors a
//     5-field payload without copying any real service contract)
//   - pattern-search: returns one of two distinct outcome shapes depending on
//     whether the query contains the "FOUND:" prefix (Action B)
//
// Both actions are read-only and deterministic. Exit code is 0 on valid
// inputs; 1 on missing required flags. Output is JSON written to stdout.
//
// Selection semantics for pattern-search outcome:
//
//	query starts with "FOUND:" → match-found outcome (matched=true, pattern, count)
//	any other query            → no-match outcome    (matched=false)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	action := flag.String("action", "", "action to invoke: status-query or pattern-search")
	target := flag.String("target", "", "target identifier (required for status-query)")
	query := flag.String("query", "", "search pattern (required for pattern-search)")
	flag.Parse()

	enc := json.NewEncoder(os.Stdout)

	switch *action {
	case "status-query":
		if *target == "" {
			fmt.Fprintln(os.Stderr, "ops-mock: status-query requires --target")
			os.Exit(1)
		}
		// Action A: 5 distinct named fields of different types.
		// Shape: id:string, name:string, status:string, region:string, health_score:int.
		_ = enc.Encode(map[string]any{
			"id":           "svc-" + slugify(*target),
			"name":         *target,
			"status":       "operational",
			"region":       "eastus",
			"health_score": 98,
		})

	case "pattern-search":
		if *query == "" {
			fmt.Fprintln(os.Stderr, "ops-mock: pattern-search requires --query")
			os.Exit(1)
		}
		// Action B: two distinct outcome shapes.
		if strings.HasPrefix(*query, "FOUND:") {
			// Match-found outcome: payload includes pattern and count.
			_ = enc.Encode(map[string]any{
				"matched": true,
				"pattern": strings.TrimPrefix(*query, "FOUND:"),
				"count":   1,
			})
		} else {
			// No-match outcome: only matched=false.
			_ = enc.Encode(map[string]any{"matched": false})
		}

	default:
		fmt.Fprintf(os.Stderr, "ops-mock: unknown action %q (want status-query or pattern-search)\n", *action)
		os.Exit(1)
	}
}

// slugify converts s to a lowercase hyphen-safe identifier.
func slugify(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
