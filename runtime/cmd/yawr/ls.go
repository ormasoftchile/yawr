package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

func runLs(args []string) int {
	return lsMain(args, ".runbook/runs", os.Stdout)
}

// lsMain lists runs in the given directory.
//
// JSON output schema (--output=json):
//
//	[
//	  {
//	    "RunID": "string (UUID v4)",
//	    "RunbookPath": "string (relative path)",
//	    "Status": "string (pending|running|waiting|completed|failed|cancelled)",
//	    "CurrentStep": "string (step ID, may be empty)",
//	    "CurrentStepIndex": number,
//	    "Vars": object (runtime variables),
//	    "StartedAt": "string (RFC3339Nano)",
//	    "UpdatedAt": "string (RFC3339Nano)"
//	  }
//	]
func lsMain(args []string, runDir string, w io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	statusFilter := fs.String("status", "", "Filter by status: running, completed, failed, cancelled")
	since := fs.Duration("since", 7*24*time.Hour, "Show runs newer than duration (e.g. 24h)")
	output := fs.String("output", "text", "Output format: text, json")

	if err := fs.Parse(args); err != nil {
		return exitValidation
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintln(os.Stderr, "invalid output format: must be text or json")
		return exitValidation
	}

	ctx := context.Background()
	store := runstore.NewDirRunStore(runDir)

	runs, err := store.ListRuns(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ls: %v\n", err)
		return exitRuntime
	}

	cutoff := time.Now().Add(-*since)
	filtered := filterRuns(runs, *statusFilter, cutoff)

	// Sort by StartedAt descending (newest first).
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})

	switch *output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(filtered); err != nil {
			fmt.Fprintf(os.Stderr, "ls: encode json: %v\n", err)
			return exitRuntime
		}
	default:
		renderLsText(w, filtered)
	}
	return exitSuccess
}

func filterRuns(runs []engine.RunState, statusFilter string, cutoff time.Time) []engine.RunState {
	out := make([]engine.RunState, 0, len(runs))
	for _, r := range runs {
		if statusFilter != "" && string(r.Status) != statusFilter {
			continue
		}
		if !cutoff.IsZero() && r.StartedAt.Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func renderLsText(w io.Writer, runs []engine.RunState) {
	if len(runs) == 0 {
		fmt.Fprintln(w, "no runs found")
		return
	}
	// Header
	fmt.Fprintf(w, "%-36s  %-12s  %-20s  %s\n", "RUN ID", "STATUS", "STARTED", "RUNBOOK")
	fmt.Fprintln(w, strings.Repeat("-", 90))
	for _, r := range runs {
		started := r.StartedAt.Local().Format("2006-01-02 15:04:05")
		fmt.Fprintf(w, "%-36s  %-12s  %-20s  %s\n",
			r.RunID, string(r.Status), started, r.RunbookPath)
	}
}
