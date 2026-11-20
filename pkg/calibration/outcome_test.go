package calibration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireVerdict(t *testing.T, want Outcome, code string, got Outcome, why *Reason) {
	t.Helper()
	require.Equal(t, want, got)
	if code == "" {
		require.Nil(t, why)
		return
	}
	require.NotNil(t, why)
	require.Equal(t, code, why.Code)
	require.NotEmpty(t, why.Message)
	actions := map[string][2]string{
		"target_not_met":    {"change_settings", "zz_params_set"},
		"requests_failed":   {"inspect_run", "zz_logs"},
		"run_changed":       {"check_run", "zz_runs"},
		"usage_unavailable": {"check_provider", "zz_providers"},
		"step_timeout":      {"change_workload", "zz_calibrate"},
	}
	if action, ok := actions[code]; ok {
		require.Equal(t, NextAction{Code: new(action[0]), Tool: new(action[1])}, why.NextAction)
	} else {
		require.Equal(t, "cancelled", code)
		require.Equal(t, NextAction{}, why.NextAction)
	}
}

func testStepRun(endpoint string, concurrency int, samples ...measuredRequest) stepRun {
	return stepRun{
		preparedRun: preparedRun{
			Target:   Target{Run: RunReport{RunID: "a", Endpoint: endpoint, OutputForced: endpoint == Chat}},
			workload: chatWorkload(concurrency),
		},
		concurrency: concurrency, requests: samples,
	}
}

func measured(sent, lease, first, last, done time.Duration, input, output int) measuredRequest {
	return measuredRequest{
		sentAt:      epoch.Add(sent),
		completedAt: epoch.Add(done),
		Sample: Sample{
			LeaseGranted: epoch.Add(lease), SlotWait: new(lease - sent),
			FirstContentToken: epoch.Add(first), LastContentToken: epoch.Add(last),
			InputTokens: new(input), OutputTokens: new(output),
		},
	}
}

func TestRunOutcomes(t *testing.T) {
	a := measured(0, 0, 1, 3, 4, 10, 2)
	b := measured(0, 0, 2, 3, 4, 11, 2)
	serial := measured(0, 0, 5, 7, 8, 10, 2)
	short := measured(0, 0, 2, 3, 4, 9, 2)
	fewOutput := measured(0, 0, 2, 3, 4, 10, 1)
	extraOutput := measured(0, 0, 2, 3, 4, 10, 3)
	noInput := a
	noInput.InputTokens = nil
	noOutput := a
	noOutput.OutputTokens = nil
	errored := a
	errored.ErrorCode, errored.InputTokens, errored.OutputTokens = "upstream_timeout", nil, nil
	tests := []struct {
		name           string
		run            stepRun
		changed, timed bool
		outcome        Outcome
		code           string
	}{
		{name: "verified overlap", run: testStepRun(Chat, 2, a, b), outcome: "verified"},
		{name: "verified embeddings", run: testStepRun(Embeddings, 2, noOutput, noOutput), outcome: "verified"},
		{name: "queued", run: testStepRun(Chat, 2, a, serial), outcome: "queued", code: "target_not_met"},
		{name: "error before missing usage", run: testStepRun(Chat, 1, errored), outcome: "failed", code: "requests_failed"},
		{name: "short chat input", run: testStepRun(Chat, 1, short), outcome: "failed", code: "requests_failed"},
		{name: "short embedding input", run: testStepRun(Embeddings, 1, short), outcome: "failed", code: "requests_failed"},
		{name: "short forced output", run: testStepRun(Chat, 1, fewOutput), outcome: "failed", code: "requests_failed"},
		{name: "long forced output", run: testStepRun(Chat, 1, extraOutput), outcome: "failed", code: "requests_failed"},
		{name: "null input usage", run: testStepRun(Embeddings, 1, noInput), outcome: "inconclusive", code: "usage_unavailable"},
		{name: "null output usage", run: testStepRun(Chat, 1, noOutput), outcome: "inconclusive", code: "usage_unavailable"},
		{name: "failure over timeout", run: testStepRun(Chat, 1, errored, measuredRequest{}), timed: true, outcome: "failed", code: "requests_failed"},
		{name: "length miss over timeout", run: testStepRun(Chat, 1, short, measuredRequest{}), timed: true, outcome: "failed", code: "requests_failed"},
		{name: "run changed over length miss", run: testStepRun(Chat, 1, short), changed: true, outcome: "inconclusive", code: "run_changed"},
		{name: "run changed over timeout", run: testStepRun(Chat, 1, measuredRequest{}), changed: true, timed: true, outcome: "inconclusive", code: "run_changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := summarizeRun(tt.run, stepObservations{}, tt.changed, tt.timed)
			requireVerdict(t, tt.outcome, tt.code, result.Outcome, result.Reason)
			if tt.name == "error before missing usage" {
				require.Contains(t, result.Reason.Message, "upstream_timeout")
				require.Equal(t, RequestCounts{Requested: 1, Failed: 1}, result.Requests)
			}
			if tt.run.Run.Endpoint == Embeddings {
				require.Nil(t, result.ConcurrentStreamsPeak)
				require.Nil(t, result.OutputTokens)
			}
		})
	}

	unforced := testStepRun(Chat, 1, fewOutput)
	unforced.Run.OutputForced = false
	result, _ := summarizeRun(unforced, stepObservations{}, false, false)
	requireVerdict(t, "verified", "", result.Outcome, result.Reason)
}

func TestWarmupOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		short, long Sample
		outcome     Outcome
		code        string
	}{
		{"missing longer usage", Sample{InputTokens: new(20)}, Sample{}, "inconclusive", "usage_unavailable"},
		{"flat counts", Sample{InputTokens: new(20)}, Sample{InputTokens: new(20)}, "inconclusive", "usage_unavailable"},
		{"falling counts", Sample{InputTokens: new(20)}, Sample{InputTokens: new(15)}, "inconclusive", "usage_unavailable"},
		{"request failure", Sample{ErrorCode: "upstream_timeout"}, Sample{InputTokens: new(90)}, "failed", "requests_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner, _, targets := runnerFixture(Chat)
			sends := 0
			runner.Sender = senderFunc(func(_ context.Context, _ Clock, _, _ string, _ map[string]any) Sample {
				sends++
				switch sends {
				case 1:
					return tt.short
				case 2:
					return tt.long
				default:
					t.Error("sent a measured request without a usable warm-up")
					return Sample{}
				}
			})
			report, err := runner.Run(t.Context(), testRequest(chatWorkload(1)), targets)
			require.NoError(t, err)
			require.Equal(t, 2, sends)
			requireVerdict(t, tt.outcome, tt.code, report.Outcome, report.Reason)
			require.Equal(t, RequestCounts{Requested: 10}, report.Steps[0].Runs[0].Requests)
		})
	}
}

func TestStepOutcomes(t *testing.T) {
	ok := RunStep{Outcome: "verified"}
	queue := RunStep{Outcome: "queued", Reason: reason("target_not_met")}
	failure := RunStep{Outcome: "failed", Reason: reason("requests_failed")}
	cut := RunStep{Outcome: "inconclusive", Reason: reason("run_changed")}
	for _, tt := range []struct {
		name    string
		runs    []RunStep
		outcome Outcome
		code    string
	}{
		{"all verified", []RunStep{ok, ok}, "verified", ""},
		{"one queued", []RunStep{ok, queue}, "queued", "target_not_met"},
		{"one inconclusive", []RunStep{ok, cut, queue}, "inconclusive", "run_changed"},
		{"one failed", []RunStep{cut, failure, ok}, "failed", "requests_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, why := stepOutcome(tt.runs)
			requireVerdict(t, tt.outcome, tt.code, got, why)
		})
	}
}

func TestOverallOutcomes(t *testing.T) {
	ok := StepReport{Outcome: "verified"}
	queue := StepReport{Outcome: "queued", Reason: reason("target_not_met")}
	failure := StepReport{Outcome: "failed", Reason: reason("requests_failed")}
	cut := StepReport{Outcome: "inconclusive", Reason: reason("step_timeout")}
	for _, tt := range []struct {
		name    string
		steps   []StepReport
		cancel  bool
		outcome Outcome
		code    string
	}{
		{"verified", []StepReport{ok, ok}, false, "verified", ""},
		{"partial with queue", []StepReport{queue, ok}, false, "partial", "target_not_met"},
		{"partial with failure", []StepReport{failure, ok}, false, "partial", "requests_failed"},
		{"partial with cut", []StepReport{ok, cut}, false, "partial", "step_timeout"},
		{"failed", []StepReport{cut, failure}, false, "failed", "requests_failed"},
		{"inconclusive", []StepReport{queue, cut}, false, "inconclusive", "step_timeout"},
		{"all queued", []StepReport{queue, queue}, false, "failed", "target_not_met"},
		{"cancelled", []StepReport{ok, failure}, true, "cancelled", "cancelled"},
		{"cancelled before measuring", nil, true, "cancelled", "cancelled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, why := reportOutcome(tt.steps, tt.cancel)
			requireVerdict(t, tt.outcome, tt.code, got, why)
		})
	}
}
