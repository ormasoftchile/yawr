.PHONY: bench

bench:
go test -run=^$$ -bench=BenchmarkEngine -benchmem -count=5 ./pkg/gxl/... ./pkg/gis ./pkg/gcp/... ./pkg/gdp ./pkg/pjvm
