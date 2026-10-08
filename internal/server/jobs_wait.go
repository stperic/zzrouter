package server

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/routing"
)

func waitRoutedJob(ctx context.Context, router routing.Router, id, node string) error {
	return waitJobCompletion(ctx, func(ctx context.Context) (jobs.Event, error) {
		ev, err := routeAndParse[jobs.Event](ctx, router, "GET", "/zzrouter/v1/internal/jobs/"+url.PathEscape(id), node, nil)
		if err != nil {
			return jobs.Event{}, fmt.Errorf("wait for job %s on %s: %w", id, node, err)
		}
		return *ev, nil
	})
}

func waitJobCompletion(ctx context.Context, lookup func(context.Context) (jobs.Event, error)) error {
	ticker := time.NewTicker(constants.StatusPollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, err := lookup(ctx)
		if err != nil {
			return err
		}
		switch ev.Phase {
		case jobs.PhaseDone:
			return nil
		case jobs.PhaseFailed:
			return fmt.Errorf("job %s failed: %s", ev.JobID, ev.Err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
