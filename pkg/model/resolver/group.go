package resolver

import (
	"context"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Group resolves model names by first checking model groups,
// then falling back to an inner Resolver for direct names.
type Group struct {
	groupStore *group.GroupStore
	fallback   Resolver
}

// NewGroup returns a Group that consults groupStore first, then
// delegates to fallback for direct (non-group) model names.
func NewGroup(groupStore *group.GroupStore, fallback Resolver) *Group {
	return &Group{
		groupStore: groupStore,
		fallback:   fallback,
	}
}

// Resolve maps a model name to a routing target.
//
// Resolution order:
//  1. If nodeHint is provided, delegate to fallback (explicit node pin).
//  2. Check groupStore for modelName. If found, return the highest-priority replica.
//  3. If not found, delegate to fallback (direct-model path).
func (r *Group) Resolve(ctx context.Context, modelName string, nodeHint string) (*Resolved, error) {
	if nodeHint != "" {
		return r.fallback.Resolve(ctx, modelName, nodeHint)
	}

	group := r.groupStore.Get(modelName)
	if group == nil {
		return r.fallback.Resolve(ctx, modelName, nodeHint)
	}

	rep := group.Replicas[0]
	utils.LogDebugf("[GroupResolver] %q resolved via group → replica %q (app=%s, model=%s, priority=%d)",
		modelName, rep.Name, rep.App, rep.Model, rep.Priority)

	return &Resolved{
		OriginalName: modelName,
		ModelName:    rep.Model,
		Node:         rep.Node,
		Provider:     rep.App,
		GroupName:    modelName,
		Strategy:     string(group.Strategy),
		Candidates:   replicasToCandidates(group.Replicas),
	}, nil
}

// replicasToCandidates converts YAML-configured replicas into the
// fallback-layer candidate representation used by routing strategies
// and the proxy.
func replicasToCandidates(reps []group.Replica) []fallback.Candidate {
	result := make([]fallback.Candidate, len(reps))
	for i, r := range reps {
		result[i] = fallback.Candidate{
			Name:       r.Name,
			Model:      r.Model,
			App:        r.App,
			Node:       r.Node,
			Priority:   r.Priority,
			Timeout:    r.Timeout.Duration,
			OnDemand:   r.OnDemand,
			MaxRetries: r.MaxRetries,
			Tags:       r.Tags,
		}
	}
	return result
}
