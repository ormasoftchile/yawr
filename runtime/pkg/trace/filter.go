package trace

// TraceFilter is a predicate for filtering trace events during reads.
type TraceFilter struct {
	// Kinds restricts to these event kinds. Empty means all kinds.
	Kinds []EventKind

	// StepID restricts to events referencing this step ID.
	// Matched against payload["step_id"]. Empty means all steps.
	StepID string

	// AfterSeq returns only events with sequence > this value.
	AfterSeq int64

	// BeforeSeq returns only events with sequence < this value. 0 means no upper bound.
	BeforeSeq int64
}

// Matches returns true if the event passes the filter.
func (f TraceFilter) Matches(event TraceEvent) bool {
	if f.AfterSeq > 0 && event.Sequence <= f.AfterSeq {
		return false
	}
	if f.BeforeSeq > 0 && event.Sequence >= f.BeforeSeq {
		return false
	}
	if len(f.Kinds) > 0 {
		matched := false
		for _, k := range f.Kinds {
			if event.Kind == k {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// StepID filtering requires payload inspection; done in implementation.
	return true
}
