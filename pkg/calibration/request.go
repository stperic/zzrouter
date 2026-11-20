// Package calibration measures workloads against checked runs without routing public requests.
package calibration

import (
	"fmt"
	"strings"
	"time"
)

const (
	// MaxRuns bounds the number of active and resident runs together.
	MaxRuns = 3
	// MaxSteps bounds the number of points in a workload's concurrency schedule.
	MaxSteps = 8
	// MaxConcurrency bounds each run's in-flight requests per step.
	MaxConcurrency = 256
	// MaxRequestsPerStep bounds each run's sample count.
	MaxRequestsPerStep = 1024
	// MaxStepTimeoutSeconds bounds the measurement window for each step.
	MaxStepTimeoutSeconds = 1800
	// DefaultStepTimeoutSeconds applies when the request omits a timeout.
	DefaultStepTimeoutSeconds = 600
)

// Request describes a calibration as submitted, before applying defaults.
type Request struct {
	Node               string       `json:"node"`
	StepTimeoutSeconds *int         `json:"step_timeout_seconds,omitempty"`
	Runs               []RunRequest `json:"runs"`
}

// RunRequest identifies a workload or a resident run to observe.
type RunRequest struct {
	RunID          string    `json:"run_id"`
	TargetRevision *string   `json:"target_revision,omitempty"`
	Workload       *Workload `json:"workload,omitempty"`
}

// Workload describes the lengths and in-flight request count to measure.
type Workload struct {
	InputTokens     int   `json:"input_tokens"`
	OutputTokens    *int  `json:"output_tokens,omitempty"`
	Concurrency     []int `json:"concurrency"`
	RequestsPerStep *int  `json:"requests_per_step,omitempty"`
}

// FieldError identifies the invalid request field for a submission refusal.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// Validate checks request fields that are independent of the runs' live state.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Node) == "" {
		return &FieldError{"node", "A node is required."}
	}
	if len(r.Runs) == 0 || len(r.Runs) > MaxRuns {
		return &FieldError{"runs", fmt.Sprintf("Specify between 1 and %d runs.", MaxRuns)}
	}
	if n := r.StepTimeoutSeconds; n != nil && (*n < 1 || *n > MaxStepTimeoutSeconds) {
		return &FieldError{"step_timeout_seconds", fmt.Sprintf("Specify between 1 and %d seconds.", MaxStepTimeoutSeconds)}
	}
	seen := make(map[string]bool, len(r.Runs))
	var active []bool
	for i, run := range r.Runs {
		path := fmt.Sprintf("runs[%d]", i)
		if strings.TrimSpace(run.RunID) == "" || seen[run.RunID] {
			return &FieldError{path + ".run_id", "Each run must have a distinct, non-empty ID."}
		}
		seen[run.RunID] = true
		if w := run.Workload; w != nil {
			if err := w.validate(path + ".workload"); err != nil {
				return err
			}
			if active == nil {
				active = make([]bool, len(w.Concurrency))
			}
			if len(active) != len(w.Concurrency) {
				return &FieldError{path + ".workload.concurrency", "Every workload must have the same number of steps."}
			}
			for step, n := range w.Concurrency {
				active[step] = active[step] || n > 0
			}
		}
	}
	if len(active) == 0 {
		return &FieldError{"runs", "At least one run must have a workload."}
	}
	for i, hasWork := range active {
		if !hasWork {
			return &FieldError{"runs", fmt.Sprintf("Step %d must have at least one active run.", i)}
		}
	}
	return nil
}

func (w Workload) validate(path string) error {
	if w.InputTokens < 1 {
		return &FieldError{path + ".input_tokens", "Input tokens must be at least 1."}
	}
	if w.OutputTokens != nil && *w.OutputTokens < 1 {
		return &FieldError{path + ".output_tokens", "Output tokens must be at least 1."}
	}
	if len(w.Concurrency) == 0 || len(w.Concurrency) > MaxSteps {
		return &FieldError{path + ".concurrency", fmt.Sprintf("Specify between 1 and %d steps.", MaxSteps)}
	}
	highest := 0
	for i, n := range w.Concurrency {
		if n < 0 || n > MaxConcurrency {
			return &FieldError{fmt.Sprintf("%s.concurrency[%d]", path, i), fmt.Sprintf("Concurrency must be between 0 and %d.", MaxConcurrency)}
		}
		highest = max(highest, n)
	}
	if count := w.RequestsPerStep; count != nil {
		switch {
		case *count < 1 || *count > MaxRequestsPerStep:
			return &FieldError{path + ".requests_per_step", fmt.Sprintf("Specify between 1 and %d requests.", MaxRequestsPerStep)}
		case *count < highest:
			return &FieldError{path + ".requests_per_step", fmt.Sprintf("Specify at least %d requests to match the highest step concurrency.", highest)}
		}
	}
	return nil
}

func (w Workload) requestCount(concurrency int) int {
	if w.RequestsPerStep != nil {
		return *w.RequestsPerStep
	}
	return max(3*concurrency, 10)
}

func (r Request) stepTimeout() time.Duration {
	seconds := DefaultStepTimeoutSeconds
	if r.StepTimeoutSeconds != nil {
		seconds = *r.StepTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}
