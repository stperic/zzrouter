package server

import (
	"context"
	"fmt"
	"slices"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/protocol"

	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/model/cache"
	"github.com/stperic/zzrouter/pkg/model/resolver"
)

func (s *Server) catalogTarget(ctx context.Context, model, provider, node string) *cache.CachedModel {
	if s.model == nil || s.model.Cache == nil {
		return nil
	}
	if node == "" || node == "localhost" {
		node = s.node.Nodename()
	}
	m, err := s.model.Cache.LookupTarget(ctx, model, provider, node)
	if err != nil {
		return nil
	}
	return m
}

func (s *Server) modelWireMode(m *cache.CachedModel, native, compat string) wireEndpointMode {
	if eps, reported := m.WireEndpoints(); reported {
		if slices.Contains(eps, native) {
			return wireModeNative
		}
		if compat != "" && slices.Contains(eps, compat) {
			return wireModeCompat
		}
		return wireModeUnsupported
	}
	if m == nil {
		return wireModeUnknown
	}
	return s.providerWireMode(m.Provider, native, compat)
}

func (s *Server) targetWireMode(ctx context.Context, model, provider, node, native, compat string) wireEndpointMode {
	if m := s.catalogTarget(ctx, model, provider, node); m != nil {
		return s.modelWireMode(m, native, compat)
	}
	return s.providerWireMode(provider, native, compat)
}

// narrowToVision removes known ineligible replicas before endpoint selection
// and cooldown tracking. Unknown replicas remain usable when none is present.
func (s *Server) narrowToVision(ctx context.Context, resolved *resolver.Resolved) bool {
	if len(resolved.Candidates) == 0 {
		state := s.catalogTarget(ctx, resolved.ModelName, resolved.Provider, resolved.Node).FeatureState("vision")
		return state == "present" || state == "unknown"
	}
	var present, unknown []fallback.Candidate
	for _, cand := range resolved.Candidates {
		switch s.catalogTarget(ctx, cand.Model, cand.App, cand.Node).FeatureState("vision") {
		case "present":
			present = append(present, cand)
		case "unknown":
			unknown = append(unknown, cand)
		}
	}
	kept := present
	if len(kept) == 0 {
		kept = unknown
	}
	if len(kept) == 0 {
		return false
	}
	resolved.Candidates = kept
	resolved.ModelName, resolved.Provider, resolved.Node = kept[0].Model, kept[0].App, kept[0].Node
	return true
}

// admitImages runs before protocol selection and fallback, while a refusal can
// still be returned as an HTTP error rather than an already-open stream.
func (s *Server) admitImages(c *gin.Context, model string, body []byte, resolved *resolver.Resolved) bool {
	if !protocol.HasImages(body) || s.narrowToVision(c.Request.Context(), resolved) {
		return true
	}
	message := fmt.Sprintf("model %q has no replica ready for image input; enable its vision feature and restart it, or choose a vision-capable model", model)
	recordErrorOnContext(c.Request.Context(), "invalid_request_error", message)
	httperr.FromContext(c).BadRequest(c, message, "model")
	return false
}

// responseAffinityTarget uses a group's sole local model on the pinned provider.
// Provider-only affinity cannot identify a model among multiple matching replicas;
// that evidence remains unknown, without redirecting a stateful continuation.
func (s *Server) responseAffinityTarget(model, provider string) *resolver.Resolved {
	target := &resolver.Resolved{ModelName: model, Provider: provider}
	if s.model == nil || s.model.Groups == nil {
		return target
	}
	group := s.model.Groups.Get(model)
	if group == nil {
		return target
	}
	var matched bool
	for _, rep := range group.Replicas {
		if rep.App != provider || (rep.Node != "" && rep.Node != "localhost" && rep.Node != s.node.Nodename()) {
			continue
		}
		if matched {
			return &resolver.Resolved{ModelName: model, Provider: provider}
		}
		target.ModelName, target.Node = rep.Model, rep.Node
		matched = true
	}
	return target
}
