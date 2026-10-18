package logs

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoMatch is returned by ResolveRuns when no runs match the filter.
var ErrNoMatch = errors.New("no runs match filter; a pin with no run has no logs")

// ErrRunNotFound is returned by GetRun when no run has the given ID or
// ID prefix.
var ErrRunNotFound = errors.New("run not found; a pin with no run has no logs")

// ErrAmbiguousPrefix is returned by GetRun when an ID prefix matches
// multiple runs. The Candidates field lists the matching run IDs so the
// caller can present a disambiguation UI (picker or CLI listing).
type ErrAmbiguousPrefix struct {
	Prefix     string
	Candidates []string
}

func (e *ErrAmbiguousPrefix) Error() string {
	return fmt.Sprintf(
		"run id prefix %q matches %d runs: %s",
		e.Prefix, len(e.Candidates), strings.Join(e.Candidates, ", "),
	)
}
