package server

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/utils"
)

func (s *DeploymentsService) restartAfterDeploy(d *Deployment, plans map[string]deployPlan) (string, error) {
	h, err := s.refresher.jobs.StartDetached(jobs.KindRun, "", jobs.Meta{"deployment": d.ID, "phase": "waiting_for_deploy"})
	if err != nil {
		return "", err
	}
	go func() {
		defer utils.RecoverAndLog("server.restartAfterDeploy")
		ctx := h.Context()
		complete := map[string]bool{}
		var failed []error
		for _, node := range d.Nodes {
			var err error
			if node.JobID != "" {
				err = waitRoutedJob(ctx, s.router, node.JobID, node.Node)
			} else if node.Status != constants.StatusCompleted {
				err = fmt.Errorf("deployment on %s has no successful job: %s", node.Node, node.Status)
			}
			if err != nil {
				failed = append(failed, err)
			} else {
				complete[node.Node] = true
			}
		}
		if err := ctx.Err(); err != nil {
			h.Fail(err)
			return
		}
		listed, err := s.refresher.runs.ListRuns(ctx, &ListRunsRequest{WithParametersStatus: true})
		if err != nil {
			h.Fail(err)
			return
		}
		out := &RunsReport{Stale: []RunRef{}}
		cfg := s.appsConfig()
		for _, run := range listed.Data {
			plan, target := plans[run.Node]
			if !target || !complete[run.Node] || run.ParametersStatus == nil || run.Status.IsTerminal() || run.ParametersStatus.Provider != plan.Provider {
				continue
			}
			svc, ok := cfg.LookupApp(plan.Provider)
			if !ok {
				continue
			}
			weights, _, _ := strings.Cut(svc.WeightsOf(run.Model), "#")
			want := d.Model
			if plan.Download != nil {
				want = plan.Download.Repo
			}
			if weights != want {
				continue
			}
			classify(out, run, true)
		}
		if len(out.Stale) > 0 {
			h.Meta(jobs.Meta{"phase": "restarting"})
			if err := s.refresher.restartRuns(ctx, out.Stale, h); err != nil {
				failed = append(failed, err)
			}
		}
		if len(failed) > 0 {
			h.Fail(errors.Join(failed...))
			return
		}
		h.Done()
	}()
	return h.ID(), nil
}
