package install

// PolicyCapabilities describes authority without exposing credentials or writable policy paths.
type PolicyCapabilities struct {
	ReleaseRecipe     bool           `json:"release_recipe"`
	OverrideApproved  bool           `json:"override_approved"`
	PolicyFingerprint string         `json:"policy_fingerprint,omitempty"`
	Grant             *RuntimePolicy `json:"grant,omitempty"`
	Reason            string         `json:"reason,omitempty"`
}
