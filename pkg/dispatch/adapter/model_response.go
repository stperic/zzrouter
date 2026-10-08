package adapter

// ShowModelResponse represents detailed model information returned by
// ModelService.ShowModel. Protocol adapters translate this canonical shape
// into their wire-format show responses.
type ShowModelResponse struct {
	// Basic model info (extracted for convenience)
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Node        string `json:"node"`
	SourceRepo  string `json:"source_repo"`
	Format      string `json:"format,omitempty"`
	ModifiedAt  string `json:"modified_at,omitempty"`
	AssignedApp string `json:"assigned_app,omitempty"`
	Digest      string `json:"digest,omitempty"`

	// All provider-specific details are stored here
	Details map[string]any `json:"details,omitempty"`

	// Route is populated by handlers when the requested model resolves
	// through an auto-route group (≥2 replicas). Protocol adapters may
	// surface it as an extension field in their wire format (e.g.,
	// Ollama's `zzrouter_route`). Left nil for single-deployment models.
	Route *ModelRouteInfo `json:"-"`
}

// ModelRouteInfo describes the auto-route group a model belongs to when it
// has multiple replicas across the cluster. Surfaced by protocol adapters
// as an extension field.
type ModelRouteInfo struct {
	Strategy string   `json:"strategy"`        // e.g., "least-load", "priority"
	Replicas int      `json:"replicas"`        // total deployment count in the group
	Nodes    []string `json:"nodes"`           // nodes currently hosting replicas
	Group    string   `json:"group,omitempty"` // group alias (often "route-<model>")
}
