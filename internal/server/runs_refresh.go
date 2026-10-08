package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// restartAffected is the one value of a provider write's ?restart: restart
// the runs the write left stale.
const restartAffected = "affected"

// percentWhole is a job's progress when every step is done.
const percentWhole = 100

// RunsReport tells a provider write's caller what it means for the models
// already running: which ones would now launch differently, and which
// ones cannot say.
type RunsReport struct {
	// Stale runs were launched before the write and a restart would give
	// them what it changed.
	Stale []RunRef `json:"stale"`
	// Overridden runs are current, but hold their own value for a key
	// whose config value differs, so a write to that key does not reach
	// them, restart or not.
	Overridden []RunRef `json:"overridden,omitempty"`
	// Unknown runs are ones whose node cannot say: it has not received
	// the write, or the config would not relaunch the run. They are never
	// restarted. A model an external daemon serves was not launched from
	// config, so it is not in the report at all.
	Unknown []RunRef `json:"unknown,omitempty"`
	// RestartJobID names the job restarting the stale runs, when the
	// write asked for it. Follow it on /jobs/:id.
	RestartJobID string `json:"restart_job_id,omitempty"`
	// Error says why the runs could not be listed. The write stands.
	Error string `json:"error,omitempty"`
}

// RunRef names one run in a RunsReport.
type RunRef struct {
	ID         string   `json:"id"`
	Node       string   `json:"node"`
	Model      string   `json:"model"`
	Changed    []string `json:"changed,omitempty"`
	Overridden []string `json:"overridden,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// reasonSyncPending is why a run on a worker the write has not reached is
// reported unknown.
const reasonSyncPending = "the node has not received this change"

// runsAPI is what a runs refresh needs of the cluster's runs.
type runsAPI interface {
	ListRuns(ctx context.Context, req *ListRunsRequest) (*ListRunsResponse, error)
	RestartRun(ctx context.Context, req *RestartRunRequest) (*RestartRunResponse, error)
}

// runsRefresher reports, and on request restarts, the runs a provider
// write affects. Each node judges its own runs against the config it
// holds, so the report is only as fresh as the sync it is given.
type runsRefresher struct {
	runs    runsAPI
	jobs    *jobs.Registry
	waitJob func(context.Context, string, string) error
	// local names this node, which holds every write it makes, so its own
	// runs never wait on a sync.
	local func() string
}

// readRestart reads a provider write's ?restart, answering 400 for any
// value but restartAffected.
func readRestart(c *gin.Context) (restart, ok bool) {
	switch v := c.Query("restart"); v {
	case "":
		return false, true
	case restartAffected:
		return true, true
	default:
		RespondWithParamErrors(c, http.StatusBadRequest, "Bad Request", []utils.ParamError{{
			Key: "restart", Code: string(httperr.CodeInvalidValue), Want: restartAffected,
			Message: fmt.Sprintf("restart=%q: the only value is %q", v, restartAffected),
		}})
		return false, false
	}
}

// report lists provider's live runs by what the write means for them and,
// when restart is set, starts a job restarting the stale ones. Nil when
// there is no refresher, which is a node with no runs API wired.
func (r *runsRefresher) report(ctx context.Context, provider string, sync SyncReport, restart bool) *RunsReport {
	if r == nil {
		return nil
	}
	out := &RunsReport{Stale: []RunRef{}}
	listed, err := r.runs.ListRuns(ctx, &ListRunsRequest{WithParametersStatus: true})
	if err != nil {
		out.Error = err.Error()
		return out
	}
	local := ""
	if r.local != nil {
		local = r.local()
	}
	for _, run := range listed.Data {
		// No status: nothing launched the run from config here, as with a
		// model an external daemon serves.
		if run.ParametersStatus == nil || run.ParametersStatus.Provider != provider || run.Status.IsTerminal() {
			continue
		}
		classify(out, run, run.Node == local || sync.Reached(run.Node))
	}
	if restart && len(out.Stale) > 0 {
		out.RestartJobID, err = r.restart(provider, out.Stale) //nolint:contextcheck // the restarts outlive the write that asked for them
		if err != nil {
			out.Error = err.Error()
		}
	}
	return out
}

// classify files one run under the report list its status puts it in.
// reached says whether the run's node holds the write being reported on.
func classify(out *RunsReport, run instance.InstanceInfo, reached bool) {
	ref := RunRef{ID: run.ID, Node: run.Node, Model: run.Model}
	status := run.ParametersStatus
	switch {
	case !reached:
		ref.Reason = reasonSyncPending
		out.Unknown = append(out.Unknown, ref)
	case status.State == instance.ParametersStale:
		ref.Changed, ref.Overridden = status.Changed, status.Overridden
		out.Stale = append(out.Stale, ref)
	case status.State == instance.ParametersCurrent:
		if len(status.Overridden) > 0 {
			ref.Overridden = status.Overridden
			out.Overridden = append(out.Overridden, ref)
		}
	default:
		ref.Reason = status.Error
		out.Unknown = append(out.Unknown, ref)
	}
}

// restart starts the job restarting runs, one at a time, and returns its
// ID. A restart stops a run before relaunching it, so the job fails if
// any did not relaunch, naming each.
func (r *runsRefresher) restart(provider string, runs []RunRef) (string, error) {
	h, err := r.jobs.StartDetached(jobs.KindRun, "", jobs.Meta{"provider": provider, "restarting": len(runs)})
	if err != nil {
		return "", fmt.Errorf("start restart job: %w", err)
	}
	go func() {
		defer utils.RecoverAndLog("server.runsRefresher.restart")
		if err := r.restartRuns(h.Context(), runs, h); err != nil {
			h.Fail(err)
			return
		}
		h.Done()
	}()
	return h.ID(), nil
}

// restartRuns is shared by provider writes and deploy chains; its caller owns
// the context and terminal emission, so cancellation cannot leave a child behind.
func (r *runsRefresher) restartRuns(ctx context.Context, runs []RunRef, h jobs.Handle) error {
	var failed []error
	restarted := map[string]string{}
	for i, run := range runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := r.runs.RestartRun(ctx, &RestartRunRequest{RunID: run.ID, Node: run.Node})
		if err == nil && resp.JobID != "" {
			if r.waitJob == nil {
				err = fmt.Errorf("restart readiness waiter is unavailable")
			} else {
				err = r.waitJob(ctx, resp.JobID, run.Node)
			}
		}
		if err != nil {
			slog.Warn("restart of a stale run failed", "run", run.ID, "node", run.Node, "error", err)
			failed = append(failed, fmt.Errorf("%s on %s: %w", run.ID, run.Node, err))
		} else {
			restarted[run.ID] = resp.ID
		}
		h.Meta(jobs.Meta{"restarted": maps.Clone(restarted)})
		h.Progress((i+1)*percentWhole/len(runs), "restarted "+run.ID, jobs.Bytes{})
	}
	return errors.Join(failed...)
}
