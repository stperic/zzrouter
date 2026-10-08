package resolver

import "context"

// Default resolves model names using the supplied RemoteLookup,
// without consulting model groups. Use this directly for cache-only
// resolution, or wrap it with Group for group-first resolution.
type Default struct {
	lookup RemoteLookup
}

// NewDefault returns a Default backed by the given remote lookup.
func NewDefault(lookup RemoteLookup) *Default {
	return &Default{lookup: lookup}
}

// Resolve maps a model name to a routing target using the remote lookup.
// If the model is found on a remote node, Node and Provider are populated.
// If the model is local or not found, Node is empty (caller handles locally).
func (r *Default) Resolve(_ context.Context, modelName string, nodeHint string) (*Resolved, error) {
	host, provider := r.lookup(modelName, nodeHint)
	return &Resolved{
		OriginalName: modelName,
		ModelName:    modelName,
		Node:         host,
		Provider:     provider,
	}, nil
}
