package instance

// ChangesCatalog reports whether this operation affects model serving evidence.
// Node-scoped runs have no model, but their admission can evict model runs.
func ChangesCatalog(model string, evicted []EvictedRun) bool {
	if model != "" {
		return true
	}
	for _, run := range evicted {
		if run.Model != "" {
			return true
		}
	}
	return false
}
