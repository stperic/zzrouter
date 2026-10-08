package server

import "github.com/stperic/zzrouter/pkg/fallback"

// filterCandidatesByProvider returns the subset of candidates whose App
// matches the given provider key.
//
// This is the enforcement point for the /api/* architectural invariant:
// the Ollama-compat surface routes only to Ollama providers. A single
// A resolver.Resolved may carry candidates across multiple providers (e.g., when
// a model is served by both an Ollama instance and a vLLM instance); /api/*
// narrows that set before handing off to the fallback proxy so non-Ollama
// backends never receive Ollama-protocol requests.
//
// The function is a caller-side filter, not a resolver concern: protocol-
// surface policy belongs at the protocol surface, not inside the generic
// resolution layer.
func filterCandidatesByProvider(candidates []fallback.Candidate, provider string) []fallback.Candidate {
	if len(candidates) == 0 {
		return candidates
	}
	out := make([]fallback.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.App == provider {
			out = append(out, c)
		}
	}
	return out
}
