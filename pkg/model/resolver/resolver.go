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
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/pricing"
)

// Reference carries the client's model spelling and separately trusted routing hints.
type Reference struct {
	// Inference opts direct requests into local target binding before wire selection.
	Inference bool
	SourceID  string
	// Exact selects physical management names, including incomplete weights.
	Exact                           bool
	Model, Provider, Node, Registry string
	// DefaultProvider scopes direct names without bypassing group resolution.
	DefaultProvider string
}

// Resolved is the result of resolving a client-facing model name to a
// concrete routing target.
type Resolved struct {
	// Quotes snapshot admitted rates for the remaining concrete targets.
	Quotes []pricing.Quote
	// OriginalName is what the client requested (e.g., "fast-chat" or "llama3.3:70b")
	OriginalName string

	// ModelName is the actual model identifier for the target provider
	ModelName string

	// RequestModel is the addressed catalog identity, preserving variant request defaults.
	RequestModel string

	// Node is the target node ("" = local, hostname = remote)
	Node string

	// Provider is the app key from provider config (e.g., "ollama", "vllm", "groq")
	Provider string

	// Replica is the selected read-only catalog identity, nil for an uncataloged name or group.
	Replica *cache.CachedModel

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
	Resolve(ctx context.Context, ref Reference) (*Resolved, error)
}
