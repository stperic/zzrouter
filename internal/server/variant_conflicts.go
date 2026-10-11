package server

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/prov_apps"
)

func (s *Server) checkCatalogModelAdmission(ctx context.Context, model string) error {
	if s.model == nil || s.model.Cache == nil {
		return nil
	}
	return s.model.Cache.CheckModelAdmission(ctx, model)
}

func (s *Server) validateModel(ctx context.Context, model string) error {
	model = s.modelIdentity(model)
	if s.providers.appMgr != nil {
		return s.providers.appMgr.ValidateModel(ctx, model)
	}
	return prov_apps.AdmissionRefusal(model, s.checkCatalogModelAdmission(ctx, model))
}

// writeModelAdmissionError answers an admission refusal on an inference
// surface from the failure table; one no row names means the backend
// cannot take the request.
func writeModelAdmissionError(w http.ResponseWriter, req *http.Request, err error) {
	failure, ok := knownFailure(err)
	if !ok {
		failure = httperr.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Code: "backend_unavailable", Message: err.Error()}
	}
	if failure.Code == "" {
		failure.Code = inferenceCode(failure.Status)
	}
	recordErrorOnContext(req.Context(), failure.Code, failure.Message)
	writeError(w, req, failure)
}

// admitResolvedModels filters reserved names before feature and wire endpoint
// preference can discard an otherwise usable replica.
func (s *Server) admitResolvedModels(c *gin.Context, resolved *resolver.Resolved, nodeHint string) bool {
	if len(resolved.Candidates) == 0 {
		if err := s.validateModel(c.Request.Context(), resolved.OriginalName); err != nil {
			writeModelAdmissionError(c.Writer, c.Request, err)
			return false
		}
		return s.admitProviderInventory(c, resolved, nodeHint)
	}
	kept := make([]fallback.Candidate, 0, len(resolved.Candidates))
	var conflict error
	var unavailable *httperr.Error
	for _, candidate := range resolved.Candidates {
		if err := s.validateModel(c.Request.Context(), candidate.Model); err != nil {
			conflict = err
			continue
		}
		node := candidate.Node
		if node == "" || s.node.IsLocalNode(node) {
			node = s.node.Nodename()
		}
		if problem := s.providerInventoryFailure(node, candidate.App, false); problem != nil {
			unavailable = problem
			continue
		}
		kept = append(kept, candidate)
	}
	if len(kept) == 0 {
		if conflict == nil && unavailable != nil {
			writeError(c.Writer, c.Request, *unavailable)
			return false
		}
		writeModelAdmissionError(c.Writer, c.Request, conflict)
		return false
	}
	resolved.Candidates = kept
	resolved.ModelName, resolved.Provider, resolved.Node = kept[0].Model, kept[0].App, kept[0].Node
	return true
}

func modelAdmissionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(cache.WithVariantAdmission(c.Request.Context()))
		c.Next()
	}
}
