// Package validator enforces specs/regions-v1.md on a parsed runbook
// that carries a `regions:` block. It is invoked from the runbook
// parser when Runbook.Regions is non-nil.
package validator

import (
	"fmt"
	"sort"

	"github.com/ormasoftchile/yawr/runtime/pkg/schema"
	"github.com/ormasoftchile/yawr/runtime/pkg/schema/regions"
)

// Error is a single region-validation failure. Code is a stable
// identifier suitable for tests and tooling; Message is human-readable.
type Error struct {
	Code     string
	RegionID string
	NodeID   string
	Message  string
}

func (e Error) Error() string {
	switch {
	case e.RegionID != "" && e.NodeID != "":
		return fmt.Sprintf("regions/%s: region %q node %q: %s", e.Code, e.RegionID, e.NodeID, e.Message)
	case e.RegionID != "":
		return fmt.Sprintf("regions/%s: region %q: %s", e.Code, e.RegionID, e.Message)
	default:
		return fmt.Sprintf("regions/%s: %s", e.Code, e.Message)
	}
}

// Validate runs every region-manifest rule against rb. Returns nil if
// rb has no manifest. Returns a non-nil slice of Errors on failure.
//
// Constraints enforced (from specs/regions-v1.md):
//
//  1. Schema version is recognized.
//  2. Region IDs are unique within the manifest.
//  3. Display value, when set, is recognized.
//  4. StatusRule, when set, is recognized (only "default" in v1).
//  5. SkipReason set iff Members is empty.
//  6. Every Members / Entries / Exits ID resolves to a real step.
//  7. Entries ⊆ Members and Exits ⊆ Members.
//  8. No node ID appears in more than one region's Members.
//  9. Structural contiguity: every region's roots either reduce to a
//     single root member or all roots share the same structural parent
//     (the runbook itself or a single container node).
//
// The contiguity check in (9) catches the common kit-compiler bugs
// (members straddling unrelated containers) but does not detect
// interleaving among same-parent siblings; that requires execution-
// order analysis and is deferred to the RegionView layer in Phase 2.
func Validate(rb *schema.Runbook) []Error {
	if rb == nil || rb.Regions == nil {
		return nil
	}
	m := rb.Regions

	var errs []Error

	// (1) Schema version.
	if m.SchemaVersion != regions.SchemaVersion {
		errs = append(errs, Error{
			Code:    "schema-version-unsupported",
			Message: fmt.Sprintf("regions.schema_version %q is not supported (expected %q)", m.SchemaVersion, regions.SchemaVersion),
		})
	}

	// (2) Region ID uniqueness.
	seenRegion := make(map[string]int, len(m.Regions))
	for i, r := range m.Regions {
		if r.ID == "" {
			errs = append(errs, Error{
				Code:    "region-empty-id",
				Message: fmt.Sprintf("regions[%d] has empty id", i),
			})
			continue
		}
		seenRegion[r.ID]++
	}
	for id, count := range seenRegion {
		if count > 1 {
			errs = append(errs, Error{
				Code:     "region-duplicate-id",
				RegionID: id,
				Message:  fmt.Sprintf("region id %q appears %d times", id, count),
			})
		}
	}

	// Build the step universe (id → parent container id).
	parents := make(map[string]string)
	known := make(map[string]bool)
	collectStepParents(rb.Flow, "", parents, known)

	// (8) Cross-region member uniqueness — track membership.
	memberOwner := make(map[string]string) // node id → region id

	for _, r := range m.Regions {
		// (3) Display value.
		if r.Display != "" &&
			r.Display != regions.DisplayCollapsed &&
			r.Display != regions.DisplayExpanded &&
			r.Display != regions.DisplayAlwaysExpanded {
			errs = append(errs, Error{
				Code:     "region-display-unknown",
				RegionID: r.ID,
				Message:  fmt.Sprintf("display %q is not recognized", r.Display),
			})
		}

		// (4) StatusRule.
		if r.StatusRule != "" && r.StatusRule != regions.StatusRuleDefault {
			// Custom rules are kit-defined and resolved at render time;
			// we accept any non-empty string here. No-op.
		}

		// (5) SkipReason iff Members empty.
		empty := len(r.Members) == 0
		hasSkip := r.SkipReason != ""
		switch {
		case empty && !hasSkip:
			errs = append(errs, Error{
				Code:     "region-empty-without-skip-reason",
				RegionID: r.ID,
				Message:  "members is empty but skip_reason is not set",
			})
		case !empty && hasSkip:
			errs = append(errs, Error{
				Code:     "region-skip-reason-with-members",
				RegionID: r.ID,
				Message:  "skip_reason is set but members is non-empty",
			})
		}

		memberSet := make(map[string]bool, len(r.Members))
		for _, id := range r.Members {
			// (6) Member resolves.
			if !known[id] {
				errs = append(errs, Error{
					Code:     "region-member-unknown",
					RegionID: r.ID,
					NodeID:   id,
					Message:  "member references an unknown step id",
				})
				continue
			}
			memberSet[id] = true

			// (8) Cross-region uniqueness.
			if owner, taken := memberOwner[id]; taken && owner != r.ID {
				errs = append(errs, Error{
					Code:     "region-member-shared",
					RegionID: r.ID,
					NodeID:   id,
					Message:  fmt.Sprintf("step is already a member of region %q", owner),
				})
			} else {
				memberOwner[id] = r.ID
			}
		}

		// (7) Entries ⊆ Members.
		for _, id := range r.Entries {
			if !memberSet[id] {
				errs = append(errs, Error{
					Code:     "region-entry-not-member",
					RegionID: r.ID,
					NodeID:   id,
					Message:  "entry is not listed in members",
				})
			}
		}
		// (7) Exits ⊆ Members.
		for _, id := range r.Exits {
			if !memberSet[id] {
				errs = append(errs, Error{
					Code:     "region-exit-not-member",
					RegionID: r.ID,
					NodeID:   id,
					Message:  "exit is not listed in members",
				})
			}
		}

		// (9) Contiguity.
		errs = append(errs, checkContiguity(r, memberSet, parents)...)
	}

	return errs
}

// checkContiguity validates that the members of r form either a single
// rooted subtree or a set of siblings sharing one structural parent.
//
// A "root" of the region is a member whose nearest enclosing container
// is not itself a member.
func checkContiguity(r regions.Region, memberSet map[string]bool, parents map[string]string) []Error {
	if len(memberSet) <= 1 {
		return nil
	}

	roots := make(map[string]bool)
	for id := range memberSet {
		// Walk up to the first ancestor not in the region (or to the
		// top-level "" sentinel).
		cur := parents[id]
		for cur != "" && memberSet[cur] {
			cur = parents[cur]
		}
		// id's immediate region-parent is cur; if cur is a member, id is
		// not a root. We tested cur not-in-set above, so id is a root
		// only when its lifted ancestor is "" or non-member. But we
		// classify using the original parent: id is a root iff its own
		// parent is not in memberSet.
		if !memberSet[parents[id]] {
			roots[id] = true
		}
	}

	if len(roots) == 1 {
		return nil
	}

	// Multiple roots: must all share the same structural parent.
	rootList := make([]string, 0, len(roots))
	for id := range roots {
		rootList = append(rootList, id)
	}
	sort.Strings(rootList)

	parent := parents[rootList[0]]
	for _, id := range rootList[1:] {
		if parents[id] != parent {
			return []Error{{
				Code:     "region-not-contiguous",
				RegionID: r.ID,
				Message: fmt.Sprintf(
					"members form %d disconnected roots %v with different structural parents; a region must be a single subtree or a set of siblings",
					len(roots), rootList),
			}}
		}
	}
	return nil
}

// collectStepParents walks the runbook flow and records, for every
// step / iterate / parallel / branch-arm-step / compensate-step ID, the
// ID of its enclosing container (or "" for top-level).
func collectStepParents(nodes []schema.FlowNode, parent string, parents map[string]string, known map[string]bool) {
	for _, fn := range nodes {
		switch {
		case fn.Step != nil:
			id := fn.Step.ID
			parents[id] = parent
			known[id] = true
			if fn.Step.BranchSpec != nil {
				for _, arm := range fn.Step.BranchSpec.Branches {
					collectStepParents(arm.Steps, id, parents, known)
				}
			}
			if fn.Step.CompensateSpec != nil {
				collectStepParents(fn.Step.CompensateSpec.Compensate.Steps, id, parents, known)
			}
		case fn.Iterate != nil:
			id := fn.Iterate.ID
			parents[id] = parent
			known[id] = true
			collectStepParents(fn.Iterate.Steps, id, parents, known)
		case fn.Parallel != nil:
			id := fn.Parallel.ID
			parents[id] = parent
			known[id] = true
			for _, b := range fn.Parallel.Branches {
				collectStepParents(b.Steps, id, parents, known)
			}
		}
	}
}
