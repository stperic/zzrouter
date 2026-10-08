package server

import "github.com/stperic/zzrouter/pkg/prov_apps/install"

// Request types shared across providers controller, service, and executor.

// installRequest is the JSON body for install requests.
// Provider name comes from the URL path parameter (:name).
type installRequest struct {
	Version string `json:"version"`
	install.PlanOptions
}

// verifyStepRequest is the JSON body for step verification.
// The provider name comes from the URL path parameter (:name).
type verifyStepRequest struct {
	install.PlanOptions
	Version string `json:"version"`
	Step    int    `json:"step"`
}

// verifyRequest is the JSON body for full install verification.
// The provider name comes from the URL path parameter (:name).
type verifyRequest struct {
	Runtime string `json:"runtime,omitempty"`
	Version string `json:"version"`
}
