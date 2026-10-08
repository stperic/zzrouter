package harness

import (
	"context"
	"encoding/json"
	"fmt"
)

// LaunchRunRequest mirrors the public POST /zzrouter/v1/runs body.
// Only the fields ring2_inference exercises are typed — extend as
// later slices reach for native_config / files / parameters.
//
// Provider/Model/LaunchMode are required by the server. AutoDeploy
// chains a model-deploy job when the file isn't already in the pool;
// Node pins the run to a specific worker (otherwise coord-local).
type LaunchRunRequest struct {
	Provider   string `json:"provider"`
	LaunchMode string `json:"launch_mode"`
	Model      string `json:"model_name"`
	Endpoint   string `json:"endpoint,omitempty"`
	AutoDeploy bool   `json:"auto_deploy,omitempty"`
	Node       string `json:"node,omitempty"`
}

// LaunchRunResponse is the 202 envelope.
//
// InstanceID is the run id, and the server sends it as "id" — reading it
// as "instance_id" left the field empty on every launch, so cleanups
// guarded on it silently did nothing and instances accumulated on the
// lab between runs.
type LaunchRunResponse struct {
	JobID      string `json:"job_id"`
	InstanceID string `json:"id,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model_name,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
}

// decodeLaunchResponse reads a launch reply in either shape it comes in:
// the public envelope, which carries the run under `data`, and the
// internal one, which puts the same fields at the top level.
//
// Reading only the top level is silent when it is wrong — InstanceID
// simply stays empty, so a cleanup written as `if InstanceID != ""`
// stops nothing and the next test inherits a running instance.
//
// Everything but the job id is best-effort: a body that will not decode
// must not fail a launch that was accepted.
func decodeLaunchResponse(body []byte, jobID string) LaunchRunResponse {
	var wrapped struct {
		Data *LaunchRunResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data != nil {
		out := *wrapped.Data
		out.JobID = jobID
		return out
	}
	out := LaunchRunResponse{}
	_ = json.Unmarshal(body, &out)
	out.JobID = jobID
	return out
}

// LaunchRun POSTs /zzrouter/v1/runs with the given body, parses the
// 202 envelope, and waits for terminal phase via Jobs.Wait. Returns
// the JobOutcome so callers can inspect Phase + Events.
//
// The launch chain (auto_deploy → download → install if needed →
// run) emits multiple phase transitions before terminal; we only
// gate on terminal here. Cold-path cost on a fresh worker is
// dominated by the model download (Qwen2.5-1.5B ≈ 1.1GB), so
// pass a generous ctx deadline (5+ minutes).
func LaunchRun(ctx context.Context, c *Client, jobs *Jobs, req LaunchRunRequest) (LaunchRunResponse, JobOutcome, error) {
	if req.Provider == "" {
		return LaunchRunResponse{}, JobOutcome{}, fmt.Errorf("LaunchRun: provider is required")
	}
	if req.LaunchMode == "" {
		req.LaunchMode = "native"
	}
	if req.Model == "" {
		return LaunchRunResponse{}, JobOutcome{}, fmt.Errorf("LaunchRun: model_name is required")
	}

	resp, err := c.POST(ctx, "/zzrouter/v1/runs", req)
	if err != nil {
		return LaunchRunResponse{}, JobOutcome{}, fmt.Errorf("POST /runs: %w", err)
	}
	jobID, err := resp.JobID()
	if err != nil {
		return LaunchRunResponse{}, JobOutcome{}, fmt.Errorf("parse 202: %w (status=%d body=%s)",
			err, resp.Status, resp.Body)
	}
	out := decodeLaunchResponse(resp.Body, jobID)

	outcome, err := jobs.Wait(ctx, jobID)
	if err != nil {
		return out, outcome, fmt.Errorf("job %s: %w", jobID, err)
	}
	if outcome.Status != "done" {
		return out, outcome, fmt.Errorf("job %s terminated with phase=%q error=%q",
			jobID, outcome.Status, outcome.Error)
	}
	return out, outcome, nil
}
