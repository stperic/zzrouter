package logs

import (
	"context"
	"errors"

	utilsclient "github.com/stperic/zzrouter/internal/client/utils"
)

// GetRun resolves an exact ID before considering prefix ambiguity.
func GetRun(ctx context.Context, c *utilsclient.Client, idOrPrefix string) (Run, error) {
	if idOrPrefix == "" {
		return Run{}, ErrRunNotFound
	}

	runs, err := ResolveRuns(ctx, c, RunFilter{RunID: idOrPrefix})
	if errors.Is(err, ErrNoMatch) {
		err = ErrRunNotFound
	}
	if err != nil {
		return Run{}, err
	}
	ids := make([]string, len(runs))
	for i, run := range runs {
		if run.ID == idOrPrefix {
			return run, nil
		}
		ids[i] = run.ID
	}
	if len(runs) == 1 {
		return runs[0], nil
	}
	return Run{}, &ErrAmbiguousPrefix{Prefix: idOrPrefix, Candidates: ids}
}
