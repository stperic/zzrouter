package logs

import (
	"context"
	"strings"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

// GetRun resolves a run ID or ID prefix to a single Run. An exact
// match wins over a prefix match, so callers that already know the
// full ID do not pay the ambiguity cost.
//
// Errors:
//   - ErrRunNotFound if no run matches.
//   - *ErrAmbiguousPrefix if a prefix matches multiple runs. The
//     error contains the candidate IDs so the caller can surface a
//     picker or listing.
func GetRun(ctx context.Context, c *utilsclient.Client, idOrPrefix string) (Run, error) {
	if idOrPrefix == "" {
		return Run{}, ErrRunNotFound
	}

	all, err := c.ListInstancesCtx(ctx)
	if err != nil {
		return Run{}, err
	}

	var prefixMatches []Run
	for _, inst := range all {
		if inst.ID == idOrPrefix {
			return runFromInstance(inst), nil // exact match short-circuits
		}
		if strings.HasPrefix(inst.ID, idOrPrefix) {
			prefixMatches = append(prefixMatches, runFromInstance(inst))
		}
	}

	switch len(prefixMatches) {
	case 0:
		return Run{}, ErrRunNotFound
	case 1:
		return prefixMatches[0], nil
	default:
		ids := make([]string, len(prefixMatches))
		for i, r := range prefixMatches {
			ids[i] = r.ID
		}
		return Run{}, &ErrAmbiguousPrefix{Prefix: idOrPrefix, Candidates: ids}
	}
}
