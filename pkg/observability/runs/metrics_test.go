package runs

import (
	"context"
	"testing"
)

// TestRecord_NilSafety pins no-op behavior on nil receivers, the
// legitimate path during early-init or before the OTel meter provider
// has bound.
func TestRecord_NilSafety(t *testing.T) {
	var m *Metrics
	m.RecordStart(context.Background(), "vllm", "llama3", "10.0.0.1")
	m.RecordStop(context.Background(), "vllm", "llama3", "10.0.0.1", StopReasonUser)
	m.RecordFailure(context.Background(), "vllm", "llama3", "10.0.0.1", FailureReadiness)

	// Package-level shortcuts must also no-op when the meter provider
	// is the noop default (no instruments registered).
	RecordStart(context.Background(), "", "", "")
	RecordStop(context.Background(), "", "", "", StopReasonUser)
	RecordFailure(context.Background(), "", "", "", FailureOther)
}

// TestStopReasonAndFailureCause_ClosedEnums pins the exact wire-string
// for every closed-enum value. Drift on any of these breaks dashboards
// silently — Prometheus treats label values as opaque strings.
func TestStopReasonAndFailureCause_ClosedEnums(t *testing.T) {
	wantReasons := map[StopReason]string{
		StopReasonUser:  "user",
		StopReasonCrash: "crash",
	}
	for v, want := range wantReasons {
		if string(v) != want {
			t.Errorf("StopReason(%v) = %q, want %q", v, string(v), want)
		}
	}
	wantCauses := map[FailureCause]string{
		FailureReadiness: "readiness_fail",
		FailureLaunchErr: "launch_err",
		FailureOther:     "other",
	}
	for v, want := range wantCauses {
		if string(v) != want {
			t.Errorf("FailureCause(%v) = %q, want %q", v, string(v), want)
		}
	}
}
