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
	Replicas    func(context.Context, string, string, string) ([]*cache.CachedModel, error)
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
	if provider == "" {
		provider = ref.Provider
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
	replicas, err := r.cfg.Replicas(ctx, name, provider, lookupNode)
	if err != nil {
		return nil, httperr.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Code: string(httperr.CodeClusterUnavailable), Message: "model inventory is unavailable; retry when the cluster is reachable"}
	}
	selectReplica := func(replica *cache.CachedModel) *Resolved {
		resolved.ModelName, resolved.RequestModel, resolved.Provider = replica.Model, replica.Name, replica.Provider
		if resolved.ModelName == "" {
			resolved.ModelName = replica.Name
		}
		if !r.cfg.IsLocalNode(replica.Node) && r.cfg.CanRoute() {
			resolved.Node = replica.Node
		}
		return resolved
	}
	if nodeHint != "" {
		if !r.cfg.IsLocalNode(nodeHint) && r.cfg.CanRoute() {
			resolved.Node = nodeHint
		}
		if len(replicas) > 0 {
			return selectReplica(replicas[0]), nil
		}
		return resolved, nil
	}
	for _, replica := range replicas {
		if r.cfg.IsLocalNode(replica.Node) {
			return selectReplica(replica), nil
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
		return selectReplica(replicas[0]), nil
	}
	return resolved, nil
}
