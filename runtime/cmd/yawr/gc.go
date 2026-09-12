package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/runstore"
	"github.com/ormasoftchile/yawr/runtime/pkg/engine"
)

// terminalStatuses are the only statuses gc may delete.
var terminalStatuses = map[engine.RunStatus]bool{
	engine.RunStatusCompleted: true,
	engine.RunStatusFailed:    true,
	engine.RunStatusCancelled: true,
}

func runGc(args []string) int {
	return gcMain(args, ".runbook/runs", os.Stdout, os.Stdin)
}

func gcMain(args []string, runDir string, w io.Writer, stdin io.Reader) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	olderThan := fs.Duration("older-than", 7*24*time.Hour, "Delete runs older than duration (e.g. 24h, 168h)")
	statusFlag := fs.String("status", "completed,failed,cancelled", "Only delete runs with these statuses (comma-separated)")
	dryRun := fs.Bool("dry-run", false, "Show what would be deleted without deleting")
	force := fs.Bool("force", false, "Skip confirmation prompt")

	if err := fs.Parse(args); err != nil {
		return exitValidation
	}

	allowedStatuses := parseStatusList(*statusFlag)
	if len(allowedStatuses) == 0 {
		fmt.Fprintln(os.Stderr, "gc: at least one status must be specified")
		return exitValidation
	}

	ctx := context.Background()
	store := runstore.NewDirRunStore(runDir)

	all, err := store.ListRuns(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gc: %v\n", err)
		return exitRuntime
	}

	cutoff := time.Now().Add(-*olderThan)
	candidates := gcCandidates(all, allowedStatuses, cutoff)

	if len(candidates) == 0 {
		fmt.Fprintln(w, "gc: nothing to delete")
		return exitSuccess
	}

	if *dryRun {
		fmt.Fprintf(w, "dry-run: would delete %d run(s):\n", len(candidates))
		for _, r := range candidates {
			fmt.Fprintf(w, "  %s  %-12s  %s\n", r.RunID, string(r.Status), r.StartedAt.Local().Format("2006-01-02 15:04:05"))
		}
		return exitSuccess
	}

	if !*force {
		fmt.Fprintf(w, "about to delete %d run(s). Confirm? [y/N] ", len(candidates))
		reader := bufio.NewReader(stdin)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		if line != "y" && line != "yes" {
			fmt.Fprintln(w, "gc: aborted")
			return exitSuccess
		}
	}

	deleted := 0
	for _, r := range candidates {
		if err := store.DeleteRun(ctx, r.RunID); err != nil {
			fmt.Fprintf(os.Stderr, "gc: delete %s: %v\n", r.RunID, err)
			continue
		}
		deleted++
	}
	fmt.Fprintf(w, "gc: deleted %d run(s)\n", deleted)
	return exitSuccess
}

// gcCandidates returns runs eligible for deletion.
// SAFETY INVARIANT (D-13-03): running status is NEVER returned.
func gcCandidates(runs []engine.RunState, allowedStatuses map[engine.RunStatus]bool, cutoff time.Time) []engine.RunState {
	var out []engine.RunState
	for _, r := range runs {
		// Never delete running runs regardless of any flag.
		if r.Status == engine.RunStatusRunning {
			continue
		}
		if !allowedStatuses[r.Status] {
			continue
		}
		// Use UpdatedAt if non-zero, otherwise StartedAt.
		ref := r.UpdatedAt
		if ref.IsZero() {
			ref = r.StartedAt
		}
		// Exclusive boundary: runs updated at exactly the cutoff are NOT deleted.
		// Only runs strictly older than the cutoff (ref < cutoff) are candidates.
		if !ref.Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func parseStatusList(s string) map[engine.RunStatus]bool {
	out := make(map[engine.RunStatus]bool)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		status := engine.RunStatus(part)
		// Never allow running to be added — safety invariant.
		if status == engine.RunStatusRunning {
			continue
		}
		if terminalStatuses[status] {
			out[status] = true
		}
	}
	return out
}
