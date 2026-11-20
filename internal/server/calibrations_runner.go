package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/calibration"
	"github.com/stperic/zzrouter/pkg/discovery/resources"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/utils/clock"
)

type nodeCalibration struct {
	server   *Server
	node     string
	targets  map[string]calibration.Target
	probes   map[string]*schema.Observe
	previous *resources.Snapshot
}

func (r *nodeCalibration) run(job jobs.Handle, request calibration.Request, claim *calibrationClaim) {
	var report *calibration.Report
	var cause error
	defer func() {
		claim.Lock()
		defer claim.Unlock()
		claim.jobID = ""
		if report == nil {
			return
		}
		job.Meta(jobs.Meta{"report": *report})
		if errors.Is(cause, jobs.ErrCanceled) {
			job.Fail(jobs.ErrCanceled)
			return
		}
		job.Done()
	}()
	job.Progress(0, calibration.Checking, jobs.Bytes{})
	runner := calibration.Runner{Sender: r, Sampler: r, Clock: clock.System(),
		Progress: func(p calibration.Progress) {
			publishCalibrationProgress(job, p, r.targets)
		},
	}
	result, err := runner.Run(job.Context(), request, r.targets)
	report, cause = &result, err
}

func calibrationEndpoint(endpoint string) (path, api string) {
	switch endpoint {
	case calibration.Chat:
		return chatCompletionsPath, "chat_completions"
	case calibration.Embeddings:
		return "/v1/embeddings", "embeddings"
	default:
		return "", ""
	}
}

func (r *nodeCalibration) currentRun(id string) (*instance.Instance, string, uint64) {
	run, ok := r.server.providers.appMgr.GetInstance(id)
	if !ok {
		return nil, "", 0
	}
	revision, admitted := run.CalibrationIdentity(r.node)
	return run, revision, admitted
}

func (r *nodeCalibration) Revisions(ctx context.Context) map[string]string {
	revisions := make(map[string]string, len(r.targets))
	for id := range r.targets {
		if ctx.Err() != nil {
			break
		}
		_, revision, _ := r.currentRun(id)
		revisions[id] = revision
	}
	return revisions
}

func (r *nodeCalibration) Send(ctx context.Context, clock calibration.Clock, id, expected string, fields map[string]any) (sample calibration.Sample) {
	inst, revision, _ := r.currentRun(id)
	if revision == "" || revision != expected {
		return calibration.Sample{ErrorCode: string(httperr.CodeRunChanged)}
	}
	sample.TargetRevision = revision
	path, _ := calibrationEndpoint(r.targets[id].Run.Endpoint)
	raw, err := json.Marshal(fields)
	if err != nil {
		sample.ErrorCode = string(httperr.CodeInvalidValue)
		return sample
	}
	wait := clock.Now()
	req := instanceRequest{Context: ctx, Method: http.MethodPost, Path: path, Header: make(http.Header),
		OriginalBody: raw, Synthetic: true, OnLease: func() {
			sample.LeaseGranted = clock.Now()
			sample.SlotWait = new(sample.LeaseGranted.Sub(wait))
			sample.TargetRevision, _ = inst.CalibrationIdentity(r.node)
		},
	}
	s := r.server
	lease, target, upstream, ok := s.openRun(req, inst, nil, func(e httperr.Error) { sample.ErrorCode = e.Code })
	defer func() {
		if ctx.Err() != nil {
			sample.ErrorCode = ""
		}
	}()
	if !ok {
		return sample
	}
	defer lease.Release()
	if sample.TargetRevision != expected {
		sample.TargetRevision = ""
		sample.ErrorCode = string(httperr.CodeRunChanged)
		return sample
	}
	body, _ := s.instanceRequest(req, inst)
	if path == chatCompletionsPath {
		body, _ = wire.ForceUsageReporting(body)
	}
	resp, err := s.sendToInstance(req, lease, body, target, upstream) //nolint:contextcheck // The request lease derives from ctx.
	if err != nil {
		sample.ErrorCode = instanceSendFailure(nil, err).Code
		return sample
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		failure := httperr.NormalizeUpstreamResponse(resp, nil, inst.FailureClassifier, nil)
		sample.ErrorCode = failure.Code
		if sample.ErrorCode == "" {
			sample.ErrorCode = string(httperr.CodeUpstreamShape)
		}
		return sample
	}
	if path == chatCompletionsPath {
		measurement, err := wire.NewStreamMeasurement(clock.Now)
		if err != nil {
			sample.ErrorCode = string(httperr.CodeUpstreamShape)
			return sample
		}
		wire.CopyStream(ctx, io.Discard, resp.Body, measurement, nil)
		sample.FirstContentToken, sample.LastContentToken = measurement.FirstContentToken, measurement.LastContentToken
		sample.InputTokens, sample.OutputTokens, sample.ErrorCode = measurement.InputTokens, measurement.OutputTokens, measurement.ErrorCode
		return sample
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyLimit+1))
	if err != nil || len(payload) > upstreamBodyLimit || !json.Valid(payload) {
		sample.ErrorCode = string(httperr.CodeUpstreamShape)
		return sample
	}
	if usage := wire.ParseUsageExtended(payload); usage.HasInput {
		sample.InputTokens = new(int(usage.In))
	}
	return sample
}

type calibrationLivePool struct {
	calibration.PoolReport
	UsedMiB        *int64   `json:"used_mib"`
	FreeMiB        *int64   `json:"free_mib"`
	AvailableMiB   *int64   `json:"available_mib"`
	SwapUsedMiB    *int64   `json:"swap_used_mib"`
	UtilizationPct *float64 `json:"utilization_pct"`
	PowerW         *float64 `json:"power_w"`
	CPUPct         *float64 `json:"cpu_pct"`
}

type calibrationLiveSample struct {
	At    time.Time             `json:"at"`
	Pools []calibrationLivePool `json:"pools"`
}

func calibrationProgress(p calibration.Progress, targets map[string]calibration.Target) jobs.Meta {
	completed, total := make(map[string]int, len(p.Runs)), make(map[string]*int, len(p.Runs))
	for id, count := range p.Runs {
		completed[id], total[id] = count.Completed, knownPositive(count.Requested)
	}
	described := calibration.DescribePools(p.Snapshot.Pools, targets)
	sample := calibrationLiveSample{At: p.SampleAt, Pools: make([]calibrationLivePool, len(described))}
	for i, pool := range p.Snapshot.Pools {
		view := described[i]
		if view.GPUIdentity == nil {
			view.GPUIdentity = &calibration.GPUIdentity{Vendor: pool.Vendor}
		} else if *view.Ownership == "unknown" {
			view.Ownership = nil
		}
		sample.Pools[i] = calibrationLivePool{PoolReport: view, UsedMiB: pool.UsedMiB, FreeMiB: pool.FreeMiB,
			AvailableMiB: pool.AvailableMiB, SwapUsedMiB: pool.SwapUsedMiB, UtilizationPct: pool.UtilizationPct,
			PowerW: pool.PowerW, CPUPct: pool.CPUPct}
	}
	return jobs.Meta{"step": p.Step, "steps": p.Steps, "requests_completed": completed,
		"requests_total": total, "sample": sample}
}

func publishCalibrationProgress(job jobs.Handle, p calibration.Progress, targets map[string]calibration.Target) {
	percent := 0
	if p.Steps > 0 {
		percent = 100 * p.Step / p.Steps
	}
	job.Meta(calibrationProgress(p, targets))
	job.Progress(percent, p.Phase, jobs.Bytes{})
}
