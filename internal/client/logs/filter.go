package logs

import (
	"context"
	"sort"
	"strings"
	"time"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

// RunFilter is the input to ResolveRuns. All fields are optional; an
// empty filter matches every run. Fields combine with AND semantics.
type RunFilter struct {
	// RunID matches exactly or as a prefix. A 1-character prefix is
	// accepted; disambiguation is the caller's problem (ResolveRuns
	// may return multiple results).
	RunID string

	// Provider matches Instance.App exactly (case-insensitive).
	Provider string

	// Model matches Instance.Model exactly (case-insensitive).
	Model string

	// Node matches Instance.Node exactly (case-insensitive).
	Node string

	// Status matches Instance.Status exactly (case-insensitive).
	// Typical values: "running", "stopped".
	Status string
}

// IsEmpty reports whether the filter has no constraints.
func (f RunFilter) IsEmpty() bool {
	return f.RunID == "" && f.Provider == "" && f.Model == "" && f.Node == "" && f.Status == ""
}

// ResolveRuns fetches all runs from the server and returns those
// matching the filter, sorted with running runs first and then by
// StartedAt descending (newest first within each status group).
//
// Filters are currently applied client-side because the server does
// not (yet) accept filter query params on GET /zzrouter/v1/runs. If
// server support lands later, the filtering can migrate server-side
// without changing this function's signature.
//
// Returns ErrNoMatch when zero runs match.
func ResolveRuns(ctx context.Context, c *utilsclient.Client, f RunFilter) ([]Run, error) {
	all, err := c.ListInstancesCtx(ctx)
	if err != nil {
		return nil, err
	}

	matched := make([]Run, 0, len(all))
	for _, inst := range all {
		r := runFromInstance(inst)
		if matchesFilter(r, f) {
			matched = append(matched, r)
		}
	}

	if len(matched) == 0 {
		return nil, ErrNoMatch
	}

	sortRuns(matched)
	return matched, nil
}

// matchesFilter applies the AND of all non-empty filter fields to a
// single run.
func matchesFilter(r Run, f RunFilter) bool {
	if f.RunID != "" && !strings.HasPrefix(r.ID, f.RunID) {
		return false
	}
	if f.Provider != "" && !strings.EqualFold(r.Provider, f.Provider) {
		return false
	}
	if f.Model != "" && !strings.EqualFold(r.Model, f.Model) {
		return false
	}
	if f.Node != "" && !strings.EqualFold(r.Node, f.Node) {
		return false
	}
	if f.Status != "" && !strings.EqualFold(r.Status, f.Status) {
		return false
	}
	return true
}

// sortRuns orders runs: running first, then by StartedAt desc. Runs
// with the same bucket and timestamp preserve relative input order.
func sortRuns(runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		ri, rj := runs[i], runs[j]
		bi, bj := statusBucket(ri.Status), statusBucket(rj.Status)
		if bi != bj {
			return bi < bj
		}
		return startedAt(ri).After(startedAt(rj))
	})
}

// statusBucket returns a sort key: 0 for running, 1 for everything else.
func statusBucket(status string) int {
	if strings.EqualFold(status, "running") {
		return 0
	}
	return 1
}

// startedAt parses Instance.StartedAt best-effort. Returns zero time on
// parse failure so such runs sort to the bottom of their bucket.
func startedAt(r Run) time.Time {
	if r.StartedAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, r.StartedAt)
	if err != nil {
		return time.Time{}
	}
	return t
}
