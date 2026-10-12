package resolver

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
)

// Default resolves direct model references from the catalog's replicas.
type Default struct{ cfg DefaultConfig }

// DefaultConfig supplies catalog and node identity without owning either.
type DefaultConfig struct {
	Replicas    func(context.Context, string, string, string, string, ...cache.ReplicaSelection) ([]*cache.CachedModel, error)
	Parse       func(string) (name, provider, node string)
	Nodename    func() string
	IsLocalNode func(string) bool
	CanRoute    func() bool
}

// NewDefault returns a replica-aware direct-name resolver.
func NewDefault(cfg DefaultConfig) *Default { return &Default{cfg: cfg} }

// Resolve prefers local replicas, then a unique remote node; explicit hints bypass selection.
func (r *Default) Resolve(ctx context.Context, ref Reference) (*Resolved, error) {
	modelName, nodeHint := ref.Model, ref.Node
	name, provider, embedded := r.cfg.Parse(modelName)
	// A registry selector addresses literal catalog names/source ids, not provider-qualified grammar.
	if ref.Registry != "" {
		name, provider, embedded = modelName, ref.Provider, ""
	}
	if provider == "" {
		provider = ref.Provider
	}
	if provider == "" {
		provider = ref.DefaultProvider
	}
	if nodeHint == "" {
		nodeHint = embedded
	}
	resolved := &Resolved{OriginalName: modelName, ModelName: name, RequestModel: name, Provider: provider}
	if !r.cfg.CanRoute() {
		nodeHint = r.cfg.Nodename()
	}
	lookupNode := nodeHint
	if lookupNode != "" && r.cfg.IsLocalNode(lookupNode) {
		lookupNode = r.cfg.Nodename()
	}
	replicas, err := r.cfg.Replicas(ctx, name, provider, lookupNode, ref.Registry, cache.ReplicaSelection{Exact: ref.Exact, SourceID: ref.SourceID})
	if err != nil {
		return nil, httperr.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Code: string(httperr.CodeClusterUnavailable), Message: "model inventory is unavailable; retry when the cluster is reachable"}
	}
	selectReplica := func(replica *cache.CachedModel) (*Resolved, error) {
		for _, other := range replicas {
			if strings.EqualFold(other.Provider, replica.Provider) && strings.EqualFold(other.Node, replica.Node) && (!strings.EqualFold(other.SourceRepo, replica.SourceRepo) || !strings.EqualFold(other.SourceID, replica.SourceID)) {
				return nil, httperr.Error{Status: http.StatusConflict, Type: "invalid_request_error", Code: string(httperr.CodeModelSourceAmbiguous), Message: fmt.Sprintf("model %q has several physical sources on provider %q and node %q; use an exact source_id from GET /zzrouter/v1/models", name, replica.Provider, replica.Node)}
			}
		}
		resolved.Replica = replica
		resolved.ModelName, resolved.RequestModel, resolved.Provider = replica.Model, replica.Name, replica.Provider
		if resolved.ModelName == "" {
			resolved.ModelName = replica.Name
		}
		if !r.cfg.IsLocalNode(replica.Node) && r.cfg.CanRoute() {
			resolved.Node = replica.Node
		}
		return resolved, nil
	}
	if nodeHint != "" {
		if !r.cfg.IsLocalNode(nodeHint) && r.cfg.CanRoute() {
			resolved.Node = nodeHint
		}
		if len(replicas) > 0 {
			return selectReplica(replicas[0])
		}
		return resolved, nil
	}
	for _, replica := range replicas {
		if r.cfg.IsLocalNode(replica.Node) {
			return selectReplica(replica)
		}
	}
	nodes := map[string]*cache.CachedModel{}
	for _, replica := range replicas {
		nodes[strings.ToLower(replica.Node)] = replica
	}
	if len(nodes) > 1 {
		choices := make([]string, 0, len(nodes))
		for _, replica := range nodes {
			choices = append(choices, modelName+"@"+replica.Node)
		}
		sort.Strings(choices)
		return nil, httperr.Error{Status: http.StatusConflict, Type: "invalid_request_error", Code: string(httperr.CodeModelNodeAmbiguous), Message: fmt.Sprintf("model %q is available on several nodes; choose %s", modelName, strings.Join(choices, ", "))}
	}
	if len(replicas) > 0 {
		return selectReplica(replicas[0])
	}
	return resolved, nil
}
