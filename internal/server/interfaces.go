package server

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/routing"
)

// ModelCacheProvider defines the interface for model cache operations
type ModelCacheProvider interface {
	LookupModel(ctx context.Context, name string) (*cache.CachedModel, error)
	ListModels(ctx context.Context, host, repo, app, model string) ([]*cache.CachedModel, error)
	Invalidate()
	RouteToModelOrBroadcast(ctx context.Context, modelName, path, method string, body []byte) (*routing.Response, error)
	// TargetNodeForModel names the node RouteToModelOrBroadcast would
	// unicast to, so a caller can resolve config for that node before
	// handing the body over. Empty means the request fans out.
	TargetNodeForModel(modelName string) string
	IsLocalNode(host string) bool
}

// ModelRegistryProvider defines the interface for model registry access
type ModelRegistryProvider interface {
	RescanLocal() (map[string]any, error)
}

// InstanceProvider defines the interface for instance management
type InstanceProvider interface {
	GetInstanceByModel(modelName string) (*instance.Instance, bool)
}

// NodeInfoProvider defines the interface for host-specific information.
// Satisfied by *NodeIdentity (GetNodename, IsWorker) and *Server (GetLocalStats).
type NodeInfoProvider interface {
	NodeNamer
	GetLocalStats() map[string]any
}

// NodeNamer provides node identity queries. Satisfied by *NodeIdentity.
type NodeNamer interface {
	GetNodename() string
	IsWorker() bool
}

// AppConfigProvider defines the interface for application configuration
// This can be expanded as needed to replace reflection hacks
type AppConfigProvider interface {
	IsAppEnabled(providerType string) bool
}

// NodeValidator validates ?node= query parameters against cluster membership.
// The empty string and "*" always pass validation (meaning "all nodes").
type NodeValidator interface {
	IsKnownNode(node string) bool
	KnownNodes() []string
}

// BackendForwarder proxies requests to upstream LLM backends.
// Satisfied by *ProxyClient. Defined for future test isolation.
type BackendForwarder interface {
	ForwardToBackend(w http.ResponseWriter, req *http.Request, providerType, targetURL string, up backend.Upstream, body []byte)
	ForwardDetached(ctx context.Context, method, targetURL string, headers http.Header, up backend.Upstream, body []byte) (*http.Response, error)
}

// RunLogHandler handles run log requests. Transitional interface — the
// implementation lives on *Server because log streaming involves deep
// cluster routing.
// TODO(decompose): move log streaming to a LogsService when cluster proxy infra is extracted.
type RunLogHandler interface {
	HandleGetRunLogsPublic(c *gin.Context)
	HandleGetRunProbesPublic(c *gin.Context)
}
