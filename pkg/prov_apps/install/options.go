package install

import (
	"context"
	"fmt"
)

// PlanOptions select a declared runtime and bind execution to its preview.
type PlanOptions struct {
	Runtime        string `json:"runtime,omitempty"`
	Action         string `json:"action,omitempty"`
	Force          bool   `json:"force,omitempty"`
	ExpectedPlanID string `json:"expected_plan_id,omitempty"`
	Disposable     bool   `json:"disposable,omitempty"`

	Smoke *SmokeOptions `json:"smoke,omitempty"`
}

// SmokeOptions requests a short inference check of a registry model after verify.
type SmokeOptions struct {
	Model string `json:"model"`
}

// ErrStalePlan requires a fresh preview before numbered execution.
var ErrStalePlan = fmt.Errorf("install plan changed; preview again")

// BindOptions incorporates action and force without filesystem observations.
func (p *Plan) BindOptions(options PlanOptions) {
	if options.Action != "" {
		p.Action = options.Action
	}
	p.PlanID = BoundPlanID(p.PlanID, p.Action, options)
}

// BoundPlanID includes mutation semantics in the canonical preview identity.
func BoundPlanID(snapshotID, action string, options PlanOptions) string {
	return Fingerprint(struct {
		Snapshot, Action  string
		Force, Disposable bool
		Smoke             *SmokeOptions
	}{snapshotID, action, options.Force, options.Disposable, options.Smoke})
}

type planOptionsKey struct{}

// WithPlanOptions gives the canonical builder the same action and isolation request as execution.
func WithPlanOptions(ctx context.Context, options PlanOptions) context.Context {
	return context.WithValue(ctx, planOptionsKey{}, options)
}

// OptionsFromContext returns the current builder's typed request options.
func OptionsFromContext(ctx context.Context) PlanOptions {
	options, _ := ctx.Value(planOptionsKey{}).(PlanOptions)
	return options
}
