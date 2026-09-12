package input

import "errors"

// ErrNoProvider is returned when no provider can resolve a request.
var ErrNoProvider = errors.New("input: no provider resolved")
