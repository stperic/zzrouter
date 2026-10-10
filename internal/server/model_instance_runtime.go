// package server provides HTTP handlers for the zzrouter host server.
// Model instance runtime - on-demand container launching and management

package server

import (
	"context"
	"fmt"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/utils"
)

// mergeTiersForTarget resolves the provider tier tree for one model on
// one named node, returning the flat maps plus the tier each parameter
// came from.
//
// The node argument is the node that would RUN the model, not the node
// resolving. Passing the wrong one — or none — is worse than not
// resolving at all: the receiving node layers what arrives over its own
// walk as the request tier, the last word, so a resolution that could
// not see nodes[N] does not merely disagree with N, it outranks it.
func mergeTiersForTarget(
	appsCfg *config.AppsConfig,
	provider, node, model, endpoint string,
	params, environment map[string]string,
) (mergedParams, mergedEnv, sources map[string]string) {
	if appsCfg == nil || provider == "" || node == "" {
		return nil, nil, nil
	}
	cfg, exists := appsCfg.LookupApp(utils.NormalizeAppType(provider))
	if !exists {
		return nil, nil, nil
	}

	resolved := cfg.ResolveEndpoint(node, model, endpoint)
	mergedParams = config.FlattenParameters(resolved.Parameters)
	mergedEnv = config.FlattenEnvironment(resolved.Environment)

	sources = make(map[string]string, len(resolved.Parameters))
	for k, rv := range resolved.Parameters {
		sources[k] = rv.Tier.String()
	}

	// Request-time overlay takes precedence (Tier 4 — ephemeral).
	for k, v := range params {
		mergedParams[k] = v
		sources[k] = config.TierRequest.String()
	}
	for k, v := range environment {
		mergedEnv[k] = v
	}
	return mergedParams, mergedEnv, sources
}

// preMergeForRequest resolves the tier tree into a preview request
// bound for target, and reports which tier each value came from. The
// node that renders the preview walks its own tree too and layers what
// arrives on top, so what this writes decides the answer.
//
// Flattening is what destroys provenance: the receiving node sees only
// a map of values arriving as request parameters and can do nothing but
// call them all `request`. The coordinator owns the provider tree, so
// this is the only place that can answer "where did this value come
// from" truthfully, and the answer has to be carried out rather than
// recomputed downstream.
//
// An empty target means the caller could not say which node would run
// the model — a broadcast, or a model no node has claimed. Then nothing
// is merged and whichever node answers resolves its own tree, which is
// the right answer for a node this one cannot name.
func preMergeForRequest(appsCfg *config.AppsConfig, req *LoadModelRequest, target string) map[string]string {
	params, env, sources := mergeTiersForTarget(
		appsCfg, req.Provider, target, req.ModelName, req.Endpoint, req.Parameters, req.Environment)
	if sources == nil {
		return nil
	}
	req.Parameters = params
	req.Environment = env

	utils.LogDebugf("[PreMerge] Merged %d params + %d env vars from coordinator config for %s/%s (node=%s)",
		len(req.Parameters), len(req.Environment), req.Provider, req.ModelName, target)
	return sources
}

// localCatalogContains reads the node snapshot using the launch catalog's name match.
func (s *Server) localCatalogContains(ctx context.Context, weights string) (bool, error) {
	if s.model.Registry == nil {
		return false, fmt.Errorf("local model registry unavailable")
	}
	models, _, err := s.model.Registry.ModelSnapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, model := range models {
		if model.Name == weights || utils.FormatModelName(model.Name) == weights {
			return true, nil
		}
	}
	return false, nil
}
