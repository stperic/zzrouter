// Package resolver resolves client-facing model names to concrete
// routing targets. Two implementations ship today: Default (cache-
// backed, no groups) and Group (group-first, falls back to Default).
//
// This package is part of the pkg/model domain consolidation; see
// docs/plan_model_subsystem_consolidation.md.
package resolver

import (
	"context"

	"github.com/stperic/zzrouter/pkg/fallback"
)

// Resolved is the result of resolving a client-facing model name to a
// concrete routing target.
type Resolved struct {
	// OriginalName is what the client requested (e.g., "fast-chat" or "llama3.3:70b")
	OriginalName string

	// ModelName is the actual model identifier for the target provider
	ModelName string

	// Node is the target node ("" = local, hostname = remote)
	Node string

	// Provider is the app key from provider config (e.g., "ollama", "vllm", "groq")
	Provider string

	// GroupName is set when resolved via a model group (empty for direct models)
	GroupName string

	// Strategy is the replica selection strategy (e.g., "priority", "least-load", "fastest").
	// Empty for non-group models, defaults to "priority".
	Strategy string

	// Candidates is the ordered fallback chain derived from the group's replicas.
	// Empty for non-group models.
	Candidates []fallback.Candidate
}

// IsRemote returns true if the model should be proxied to a remote node.
func (r *Resolved) IsRemote() bool {
	return r.Node != ""
}

// Resolver resolves a client-facing model name to routing information.
// Implementations can resolve via cache (Default), model groups (Group),
// or both.
type Resolver interface {
	// Resolve maps a model name to a Resolved target.
	// nodeHint allows clients to pin requests to a specific node (via X-Node header).
	Resolve(ctx context.Context, modelName string, nodeHint string) (*Resolved, error)
}

// RemoteLookup resolves a model name to its remote host and provider.
// Supplied by the caller (typically the Server's cache) so this package
// stays decoupled from the cache implementation.
type RemoteLookup func(modelName string, nodeHint ...string) (host, provider string)
