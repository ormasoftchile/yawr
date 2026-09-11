package adapter

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/ormasoftchile/yawr/runtime/pkg/pkgcatalog"
	"github.com/ormasoftchile/yawr/runtime/pkg/trace"
)

// EmitPackageCatalogTraceEvents writes the package/resolved (one per
// resolved tier-1 package) and catalog/frozen (once) trace events for a
// just-built package catalog (design/yawr/sections/07-runtime-events.tex
// §Package and Substitution Events). Callers invoke this once per run,
// immediately after pkgcatalog.Build/BuildPackageCatalog succeeds and
// before the engine's own run/started event, using the same runID the
// run is about to start with (so package/resolved is genuinely emitted
// "before any run/started side effects", per the payload doc comment) and
// the same trace.TraceWriter the engine itself writes to
// (EngineConfig.TraceWriter), so these events land in the same JSONL trace
// file as every other runtime event -- not a side channel.
//
// ConstraintSources is populated from pkgcatalog.LockedPackageInfo's own
// ConstraintSources field, which mergeRequirements threads through from
// the project/runbook requires: scopes that actually contributed a
// version: constraint (§4.4, §7.4 of the ratified spec) -- this used to
// be emitted unconditionally empty; that was a required fix (§3.1 of
// Barbara's gate review), not an accepted gap.
//
// originByPackage optionally supplies each package's --package-map
// override provenance ("project" or "package-map", keyed by package
// name), so callers whose run went through the --package-map merge path
// (cmd/yawr/run.go's mergePackageBindings) can surface that in the trace
// record itself rather than stderr-only (§3.9). Pass nil when the run has
// no --package-map involvement at all (Origin is simply omitted then).
func EmitPackageCatalogTraceEvents(writer trace.TraceWriter, runID string, cat *pkgcatalog.Catalog, originByPackage map[string]string) error {
	if writer == nil || cat == nil {
		return nil
	}
	seq := int64(1)
	for _, pkg := range cat.Packages {
		payload, err := json.Marshal(trace.PackageResolvedPayload{
			Name:              pkg.Name,
			Version:           pkg.Version,
			Digest:            pkg.Digest,
			Root:              pkg.Root,
			External:          pkg.External,
			ConstraintSources: pkg.ConstraintSources,
			Origin:            originByPackage[pkg.Name],
		})
		if err != nil {
			return err
		}
		if err := writer.Append(trace.TraceEvent{
			EventID:   uuid.NewString(),
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			RunID:     runID,
			Kind:      trace.EventKindPackageResolved,
			Sequence:  seq,
			Payload:   payload,
		}); err != nil {
			return err
		}
		seq++
	}

	tiers := map[string]int{}
	for _, e := range cat.Entries() {
		tiers[tierName(e.Tier)]++
	}
	frozenPayload, err := json.Marshal(trace.CatalogFrozenPayload{
		CatalogDigest: cat.CatalogDigest(),
		ToolCount:     len(cat.Entries()),
		Tiers:         tiers,
	})
	if err != nil {
		return err
	}
	return writer.Append(trace.TraceEvent{
		EventID:   uuid.NewString(),
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		RunID:     runID,
		Kind:      trace.EventKindCatalogFrozen,
		Sequence:  seq,
		Payload:   frozenPayload,
	})
}

func tierName(t pkgcatalog.Tier) string {
	switch t {
	case pkgcatalog.TierBuiltin:
		return "builtin"
	case pkgcatalog.TierPackage:
		return "package"
	case pkgcatalog.TierProject:
		return "project"
	case pkgcatalog.TierDynamic:
		return "dynamic"
	case pkgcatalog.TierRunbook:
		return "runbook"
	default:
		return "unknown"
	}
}
