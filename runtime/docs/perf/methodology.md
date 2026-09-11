# P8 runtime performance methodology

Date: 2026-06-07. Baseline commit: `f98c4e0` (`feat(capture): add P7 capture service`).

## Command

`make bench` runs:

```sh
go test -run=^$ -bench=BenchmarkEngine -benchmem -count=5 ./pkg/gxl/... ./pkg/gis ./pkg/gcp/... ./pkg/gdp ./pkg/pjvm
```

The committed raw outputs are:

- `docs/perf/baseline-f98c4e0.txt`  native runtime before expr-lang deletion.
- `docs/perf/p8-cutover-f98c4e0.txt`  same suite after P8 cutover.

## Suite

- GXL: parse plus eval of representative boolean, string, list, and path expressions.
- GIS: interpolation of representative literal, expression, escape, and path templates.
- GCP/GDP: capture-path resolution over representative PJVM output trees.
- PJVM: representative Go-to-PJVM conversion.
- Full fixture: parse-gate planning of `examples/collect-health/collect-health.runbook.yaml` with eager include resolution.

The fixture suite is intentionally small because Brady deferred the final soak infrastructure decision on 2026-06-07. Runtime engineers should replace the fixture list in `internal/perf` when that decision lands.

## Thresholds

Results use the median of five runs per benchmark and aggregate category medians. Microbenchmarks have a 30% wall-time tolerance to account for non-dedicated workstation noise; the full fixture threshold is 2x. Allocation caps are <=2x bytes/op and <=1.5x allocs/op for every benchmark.

| Category | Baseline median | P8 median | Threshold | Result |
| --- | ---: | ---: | ---: | :---: |
| GXL parse/eval | 6,689 ns/op | 8,098 ns/op | <=8,696 ns/op | PASS |
| GIS interpolation | 4,486 ns/op | 5,318 ns/op | <=5,832 ns/op | PASS |
| GCP path resolution | 13,518 ns/op | 15,012 ns/op | <=17,573 ns/op | PASS |
| GDP PJVM tree resolution | 343 ns/op | 359 ns/op | <=446 ns/op | PASS |
| Full fixture parse-gated run | 36.78 ms/op | 38.84 ms/op | <=73.56 ms/op | PASS |

No benchmark exceeded its allocation cap in the P8 run.

## Required checks

- `go test ./internal/conformance -count=1 -v` must report 267/267 passing and zero skips.
- `go build ./...` must pass.
- `go test ./... -count=1` must pass.
- `go test -race ./... -count=1` must pass before merge.
- Placeholder soak: `go run ./cmd/soak --duration 5m` must complete panic-free.
