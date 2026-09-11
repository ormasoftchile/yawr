package conformance

import "context"

// Verdict is a conformance vector outcome.
type Verdict string

const (
	// VerdictPass means the vector passed.
	VerdictPass Verdict = "pass"
	// VerdictFail means the vector failed.
	VerdictFail Verdict = "fail"
	// VerdictSkip means the vector was explicitly skipped.
	VerdictSkip Verdict = "skip"
)

// Result is one conformance runner outcome.
type Result struct {
	Verdict Verdict
	Reason  string
	Err     error
}

// Runner executes one vector.
type Runner interface {
	Run(context.Context, Vector) Result
}

func skip(reason string) Result { return Result{Verdict: VerdictSkip, Reason: reason} }
