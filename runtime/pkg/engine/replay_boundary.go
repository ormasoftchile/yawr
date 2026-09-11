package engine

import (
	"errors"
	"fmt"
)

var ErrReplayBoundary = errors.New("engine: replay boundary failed")

type replayBoundaryError struct{ cause error }

func (err *replayBoundaryError) Error() string {
	return fmt.Sprintf("%v: %v", ErrReplayBoundary, err.cause)
}

func (err *replayBoundaryError) Unwrap() []error {
	return []error{ErrReplayBoundary, err.cause}
}

func NewReplayBoundaryError(cause error) error {
	if cause == nil {
		cause = errors.New("replay fixture is unavailable")
	}
	if errors.Is(cause, ErrReplayBoundary) {
		return cause
	}
	return &replayBoundaryError{cause: cause}
}

func IsReplayBoundaryError(err error) bool {
	return errors.Is(err, ErrReplayBoundary)
}
