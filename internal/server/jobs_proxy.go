// jobs_proxy.go — coord-side wiring for JobsController's cross-node
// SSE proxy. Two hooks: a streaming-tuned mTLS client factory (fresh
// per call to pick up role transitions) and an endpoint resolver
// backed by the coordinator's mesh registry.
//
// Streaming tuning is why we don't reuse node.DialClient's transport
// directly: DialClient sets no ResponseHeaderTimeout, so a half-open
// worker would wedge the proxy goroutine past any useful bound. We
// build a fresh transport atop DialClient's TLS config per call.

package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
)

// cachedJobsStreamClient returns a factory closure that lazily builds
// the streaming mTLS client once and returns the same instance on
// subsequent calls. Routes rebuild on role transition, so the cache
// ties to the current listener identity without explicit invalidation.
// Building a fresh *http.Transport per request would accumulate idle
// connection pools and TLS sessions under streaming load.
func (s *Server) cachedJobsStreamClient() func() (*http.Client, error) {
	var (
		once   sync.Once
		client *http.Client
		err    error
	)
	return func() (*http.Client, error) {
		once.Do(func() { client, err = s.newJobsStreamClient() })
		return client, err
	}
}

// newJobsStreamClient builds the streaming mTLS client. Coord dials
// workers; peer OU asserted = worker. Transport is streaming-tuned —
// ResponseHeaderTimeout + TCP keepalive close half-open peers within
// a bounded window, unlike the general DialClient transport which has
// no header deadline.
func (s *Server) newJobsStreamClient() (*http.Client, error) {
	if s.cluster.listener == nil {
		return nil, errors.New("cluster listener not initialized")
	}
	base, err := s.cluster.listener.DialClient(clusterid.RoleWorker.OU())
	if err != nil {
		return nil, fmt.Errorf("dial client: %w", err)
	}
	baseTr, ok := base.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unexpected DialClient transport type %T", base.Transport)
	}
	tr := &http.Transport{
		TLSClientConfig: baseTr.TLSClientConfig,
		DialContext: (&net.Dialer{
			Timeout:   proxyDialTimeout,
			KeepAlive: proxyKeepAlive,
		}).DialContext,
		ResponseHeaderTimeout: proxyResponseHeaderTimeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       proxyKeepAlive * 3,
	}
	return &http.Client{Transport: tr}, nil
}

// listJobsPeers returns the names of every non-local cluster peer the
// coord knows about. Used by GetJob/StreamJob fan-out so agents can
// poll a job_id without remembering its owner. Empty slice when no
// coordinator state is available — callers degrade to local-only.
func (s *Server) listJobsPeers() []string {
	if s.cluster.coordinator == nil || s.cluster.coordinator.GetState() == nil {
		return nil
	}
	reg := s.cluster.coordinator.GetState().GetEndpoints()
	if reg == nil {
		return nil
	}
	endpoints := reg.GetAllEndpoints()
	out := make([]string, 0, len(endpoints))
	self := s.node.Name()
	for _, ep := range endpoints {
		if ep == nil || ep.IsLocal {
			continue
		}
		if strings.EqualFold(ep.NodeName, self) {
			continue
		}
		if ep.NodeName != "" {
			out = append(out, ep.NodeName)
		}
	}
	return out
}

// resolveJobsEndpoint looks up an endpoint by node hint. Accepts
// nodename, alias, name, or full endpoint URL via the registry's
// indexed lookup (O(1)). Self-endpoint is filtered — handleRemote
// already short-circuits local hits via isLocalFn, but a stale hint
// that matches self here would loop the proxy.
func (s *Server) resolveJobsEndpoint(node string) *mesh.Endpoint {
	if s.cluster.coordinator == nil || s.cluster.coordinator.GetState() == nil {
		return nil
	}
	reg := s.cluster.coordinator.GetState().GetEndpoints()
	if reg == nil {
		return nil
	}
	matches := reg.GetEndpointsForRequest(&mesh.Request{TargetNode: node})
	for _, ep := range matches {
		if ep == nil || ep.IsLocal {
			continue
		}
		if strings.EqualFold(ep.NodeName, s.node.Name()) {
			continue
		}
		return ep
	}
	return nil
}
