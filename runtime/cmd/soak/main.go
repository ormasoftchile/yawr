// Command soak runs the placeholder P8 runtime soak workload.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/ormasoftchile/yawr/runtime/internal/perf"
)

func main() {
	defaultRoot, err := perf.RepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	duration := flag.Duration("duration", 5*time.Minute, "soak duration")
	repoRoot := flag.String("repo-root", defaultRoot, "repository root containing examples")
	workDir := flag.String("work-dir", filepath.Join(defaultRoot, ".soak-runs"), "directory for soak traces and run state")
	flag.Parse()

	// REPLACE-WHEN-INFRA-DECIDED: Brady deferred the real soak infrastructure
	// decision on 2026-06-07. Keep this harness small and swappable.
	stats, err := runSoak(context.Background(), *repoRoot, *workDir, *duration)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soak failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("soak clean: duration=%s iterations=%d steps=%d vectors_per_second=%.2f alloc_start=%d alloc_end=%d alloc_growth=%d peak_alloc=%d panic_free=%t\n",
		stats.Duration.Round(time.Millisecond), stats.Iterations, stats.Steps, stats.VectorsPerSecond(), stats.StartAlloc, stats.EndAlloc, int64(stats.EndAlloc)-int64(stats.StartAlloc), stats.PeakAlloc, stats.PanicFree)
}

func runSoak(ctx context.Context, repoRoot, workDir string, duration time.Duration) (stats perf.SoakStats, err error) {
	stats.PanicFree = true
	defer func() {
		if r := recover(); r != nil {
			stats.PanicFree = false
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return stats, err
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	stats.StartAlloc = mem.Alloc
	stats.PeakAlloc = mem.Alloc

	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	start := time.Now()
	var iter int64
	for ctx.Err() == nil {
		for _, fixture := range perf.FixturePaths(repoRoot) {
			if ctx.Err() != nil {
				break
			}
			iteration := atomic.AddInt64(&iter, 1)
			iterDir := filepath.Join(workDir, fmt.Sprintf("iter-%06d", iteration))
			steps, runErr := perf.RunFixtureDryRun(ctx, repoRoot, fixture, iterDir)
			if runErr != nil {
				stats.LastError = runErr
				stats.Duration = time.Since(start)
				return stats, runErr
			}
			stats.Iterations++
			stats.Steps += int64(steps)
			runtime.ReadMemStats(&mem)
			stats.EndAlloc = mem.Alloc
			if mem.Alloc > stats.PeakAlloc {
				stats.PeakAlloc = mem.Alloc
			}
		}
	}
	runtime.ReadMemStats(&mem)
	stats.EndAlloc = mem.Alloc
	if mem.Alloc > stats.PeakAlloc {
		stats.PeakAlloc = mem.Alloc
	}
	stats.Duration = time.Since(start)
	return stats, nil
}
