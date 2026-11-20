package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/affinity"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

func responseOwner(c *gin.Context) affinity.Owner {
	if trusted, _ := c.Get(string(CtxKeyClusterTrusted)); trusted == true {
		id := c.GetHeader(constants.HeaderResponseOwner)
		if decoded, err := hex.DecodeString(id); err == nil && len(decoded) == sha256.Size {
			return affinity.Owner{Kind: "cluster", ID: id}
		}
		return affinity.Owner{}
	}

	ac := GetAccessContext(c)
	if ac == nil || ac.Key == nil {
		return affinity.Owner{}
	}
	subject := ac.Key.Subject()
	return affinity.Owner{Kind: string(subject.Kind), ID: subject.ID, TeamID: subject.TeamID}
}

func responseNotFound(c *gin.Context) {
	writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusNotFound, Type: "invalid_request_error", Code: "response_not_found", Message: "Response not found for this caller"})
}

func responseOriginError(c *gin.Context, code, message string) {
	writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusConflict, Type: "invalid_request_error", Code: code, Message: message})
}

func responseOriginUnauthorized(c *gin.Context) {
	writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusUnauthorized, Type: "authentication_error", Code: "response_owner_required", Message: "Stored Responses require an authenticated caller; use store=false for stateless inference"})
}

func (s *Server) responseOriginAvailable(c *gin.Context, origin affinity.Origin) bool {
	if origin.Node != "" {
		if s.resolveNodeToClusterURL(origin.Node) != "" {
			return true
		}
	} else if origin.RunID != "" {
		if s.providers.appMgr != nil {
			if run, ok := s.providers.appMgr.Instances().Get(origin.RunID); ok && run.Provider == origin.ProviderKey && run.GetStatus() == instance.StatusRunning {
				return true
			}
		}
	} else if target, ok := s.backend.Resolve(origin.ProviderKey); ok && target.Revision == origin.ConnectionRevision && origin.ConnectionRevision != "" {
		return true
	}
	responseOriginError(c, "response_origin_unavailable", "The response's originating connection or run is no longer available")
	return false
}

// forwardResponseOrigin reuses native forwarding with a bound run or a checked connection snapshot.
func (s *Server) forwardResponseOrigin(c *gin.Context, origin affinity.Origin, body []byte) {
	if origin.Node != "" {
		s.proxyToRemoteNode(c, origin.Node, origin.ProviderKey, body)
		return
	}
	if origin.RunID != "" {
		if run, ok := s.providers.appMgr.Instances().Get(origin.RunID); ok && run.Provider == origin.ProviderKey && run.GetStatus() == instance.StatusRunning {
			c.Request = c.Request.WithContext(affinity.WithExpected(c.Request.Context(), origin))
			s.forwardToRun(c.Writer, c.Request, body, run)
			return
		}
	} else if target, ok := s.backend.Resolve(origin.ProviderKey); ok && target.Revision == origin.ConnectionRevision && origin.ConnectionRevision != "" {
		affinity.Observe(c.Request.Context(), origin)
		s.proxy.ForwardToBackend(c.Writer, c.Request, origin.ProviderKey, target.InferenceTarget(c.Request.URL.Path, c.Request.URL.RawQuery), target.Upstream, body)
		return
	}
	responseOriginError(c, "response_origin_unavailable", "The response's originating connection or run is no longer available")
}

// carryResponseOwner preserves caller isolation behind the worker's synthetic access context.
func carryResponseOwner(c *gin.Context, owner affinity.Owner) {
	if trusted, _ := c.Get(string(CtxKeyClusterTrusted)); trusted == true {
		return
	}
	encoded, _ := json.Marshal(owner)
	digest := sha256.Sum256(encoded)
	c.Request.Header.Set(constants.HeaderResponseOwner, hex.EncodeToString(digest[:]))
}

func responseOriginTarget(origin affinity.Origin) *resolver.Resolved {
	return &resolver.Resolved{OriginalName: origin.ClientModel, GroupName: origin.Route, ModelName: origin.Model, RequestModel: origin.Model, Provider: origin.ProviderKey, Node: origin.Node}
}
