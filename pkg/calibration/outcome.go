package calibration

// Outcome describes the evidence at a run, step or report level.
type Outcome string

const (
	verified     Outcome = "verified"
	queued       Outcome = "queued"
	failed       Outcome = "failed"
	inconclusive Outcome = "inconclusive"
	partial      Outcome = "partial"
	cancelled    Outcome = "cancelled"
)

const (
	targetNotMet     = "target_not_met"
	requestsFailed   = "requests_failed"
	runChanged       = "run_changed"
	usageUnavailable = "usage_unavailable"
	stepTimeout      = "step_timeout"
)

// Reason tells the caller why a result is non-verified and how to investigate.
type Reason struct {
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	NextAction NextAction `json:"next_action"`
}

// NextAction names the next management tool, if applicable.
type NextAction struct {
	Code *string `json:"code"`
	Tool *string `json:"tool"`
}

func reason(code string) *Reason {
	switch code {
	case targetNotMet:
		return reasonWithAction(code, "The requested streaming concurrency was not reached.", "change_settings", "zz_params_set")
	case requestsFailed:
		return reasonWithAction(code, "A calibration request failed or did not reach the required token length.", "inspect_run", "zz_logs")
	case runChanged:
		return reasonWithAction(code, "The run changed or its admission counter reset.", "check_run", "zz_runs")
	case usageUnavailable:
		return reasonWithAction(code, "The engine did not report the token usage needed to verify the workload.", "check_provider", "zz_providers")
	case stepTimeout:
		return reasonWithAction(code, "The step reached its time limit.", "change_workload", "zz_calibrate")
	case string(cancelled):
		return &Reason{Code: code, Message: "The calibration was cancelled."}
	default:
		return nil
	}
}

func reasonWithAction(code, message, action, tool string) *Reason {
	return &Reason{Code: code, Message: message, NextAction: NextAction{Code: &action, Tool: &tool}}
}

func requestFailure(code string) *Reason {
	r := reason(requestsFailed)
	r.Message = "A calibration request failed with code " + code + "."
	return r
}

type runEvidence struct {
	changed      bool
	timedOut     bool
	warmupReason *Reason
	errorCode    string
	lengthMiss   bool
	missingUsage bool
	queued       bool
}

func runOutcome(e runEvidence) (Outcome, *Reason) {
	switch {
	case e.changed:
		return inconclusive, reason(runChanged)
	case e.errorCode != "":
		return failed, requestFailure(e.errorCode)
	case e.lengthMiss:
		return failed, reason(requestsFailed)
	case e.warmupReason != nil && e.warmupReason.Code == requestsFailed:
		return failed, e.warmupReason
	case e.warmupReason != nil:
		return inconclusive, e.warmupReason
	case e.timedOut:
		return inconclusive, reason(stepTimeout)
	case e.missingUsage:
		return inconclusive, reason(usageUnavailable)
	case e.queued:
		return queued, reason(targetNotMet)
	default:
		return verified, nil
	}
}

func stepOutcome(runs []RunStep) (Outcome, *Reason) {
	outcome, why := verified, (*Reason)(nil)
	for _, run := range runs {
		if severity(run.Outcome) > severity(outcome) {
			outcome, why = run.Outcome, run.Reason
		}
	}
	return outcome, why
}

func reportOutcome(steps []StepReport, canceled bool) (Outcome, *Reason) {
	if canceled {
		return cancelled, reason(string(cancelled))
	}
	hasVerified := false
	worst, why := verified, (*Reason)(nil)
	for _, step := range steps {
		hasVerified = hasVerified || step.Outcome == verified
		if severity(step.Outcome) > severity(worst) {
			worst, why = step.Outcome, step.Reason
		}
	}
	switch {
	case worst == verified:
		return verified, nil
	case hasVerified:
		return partial, why
	case worst == queued:
		return failed, why
	default:
		return worst, why
	}
}

func severity(o Outcome) int {
	switch o {
	case failed:
		return 3
	case inconclusive:
		return 2
	case queued:
		return 1
	default:
		return 0
	}
}
