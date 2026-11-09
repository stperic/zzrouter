package server

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	clustersignal "github.com/stperic/zzrouter/pkg/cluster/signal"
	"github.com/stperic/zzrouter/pkg/httperr"
)

func (s *Server) handleSignalPoll(c *gin.Context) {
	if !s.node.IsWorker() || s.cluster.signaler == nil {
		NotFound(c, "node exchange unavailable")
		return
	}
	var poll clustersignal.Poll
	if !BindJSONStrict(c, &poll) {
		return
	}
	batch, err := s.cluster.signaler.Wait(c.Request.Context(), poll)
	if err != nil {
		ServiceUnavailable(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, batch)
}
func (s *Server) handleSignalAck(c *gin.Context) {
	if !s.node.IsWorker() || s.cluster.signaler == nil {
		NotFound(c, "node exchange unavailable")
		return
	}
	var ack clustersignal.Acknowledgment
	if !BindJSONStrict(c, &ack) {
		return
	}
	if !s.cluster.signaler.Acknowledge(ack) {
		Conflict(c, "node exchange incarnation changed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"acknowledged": true})
}
func (s *Server) signalMember(url, node string) bool {
	if s.cluster.coordinator == nil {
		return false
	}
	for _, ep := range s.cluster.coordinator.GetAllEndpoints() {
		if ep.IsLocal || ep.NodeName != node {
			continue
		}
		clusterURL := ep.ClusterURL
		if clusterURL == "" {
			clusterURL, _ = mesh.DeriveClusterURL(ep.URL, s.config.Cluster.BindPort)
		}
		if clusterURL == url {
			return true
		}
	}
	return false
}
func (s *Server) handleNodeOperation(ctx context.Context, cert *x509.Certificate, op clustersignal.Operation) clustersignal.Result {
	if err := s.cluster.listener.ValidateWorker(cert); err != nil {
		code := "worker_identity_refused"
		if errors.Is(err, clusternode.ErrRenewDenyListed) {
			code = "revoked"
		}
		return clustersignal.Result{Error: &clustersignal.StatusError{Status: 403, Code: code, Detail: err.Error()}}
	}
	ep := s.certifiedWorker(cert)
	if ep == nil {
		return clustersignal.Result{Error: &clustersignal.StatusError{Status: 403, Detail: "certificate does not name a cluster worker"}}
	}
	result := clustersignal.Result{ID: op.ID}
	var err error
	switch op.Kind {
	case clustersignal.Catalog:
		if s.model.Cache == nil {
			return clustersignal.Result{Error: &clustersignal.StatusError{Status: 503, Code: "catalog_unavailable", Detail: "catalog is unavailable"}}
		}
		s.RefreshClusterEndpoint(ep.URL) //nolint:contextcheck // Wake lifecycle-owned probes; publication below inherits the caller.
		err = s.model.Cache.RefreshNode(ctx, ep.NodeName)
	case clustersignal.ProviderEnabled:
		err = s.recordNodeProviderEnabled(ctx, ep.NodeName, op.Provider, op.Enabled)
	case clustersignal.Goodbye:
		s.MarkClusterEndpointDown(ep.URL)
	case clustersignal.Renew:
		result.Certificate, err = s.cluster.listener.SignRenewal(cert, op.CSR)
	default:
		result.Error = &clustersignal.StatusError{Status: 400, Code: "unknown_node_operation", Detail: "unknown node operation"}
	}
	if err != nil {
		result.Error = &clustersignal.StatusError{Status: 503, Detail: err.Error()}
		var problem httperr.Error
		if errors.As(err, &problem) {
			result.Error.Status = problem.Status
			result.Error.Code = problem.Code
			result.Error.Detail = problem.Message
		}
		if errors.Is(err, clusternode.ErrRenewDenyListed) {
			result.Error.Status = 403
			result.Error.Code = "revoked"
		}
	}
	return result
}
