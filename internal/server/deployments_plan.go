package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/utils"
)

type deployPlan struct {
	Provider string
	Features []string
	Download *metadata.DownloadRequest
}

func selectedFeatures(svc pkgConfig.ServiceConfig, requested *[]string) ([]string, error) {
	var names []string
	if requested == nil {
		for name, f := range svc.Features {
			if f.Default {
				names = append(names, name)
			}
		}
	} else {
		names = append(names, (*requested)...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		if _, ok := svc.Features[name]; !ok {
			return nil, fmt.Errorf("provider does not declare feature %q", name)
		}
	}
	return names, nil
}

func featureDownload(req *DeployRequest, svc pkgConfig.ServiceConfig, names []string) metadata.DownloadRequest {
	repo, file, _ := strings.Cut(svc.WeightsOf(req.Model), "#")
	if req.File != "" {
		file = req.File
	}
	d := metadata.DownloadRequest{Repo: repo, Weights: file, Force: req.Force, Features: map[string][]string{}}
	if req.Features == nil || req.defaultFeatures {
		d.Optional = slices.Clone(names)
	}
	for name, f := range svc.Features {
		if len(f.Files) > 0 {
			d.Features[name] = f.Files
		}
	}
	for _, name := range names {
		if len(svc.Features[name].Files) > 0 {
			d.Want = append(d.Want, name)
		}
	}
	return d
}

func (s *DeploymentsService) planDeploy(ctx context.Context, req *DeployRequest, nodes []string) (map[string]deployPlan, error) {
	cfg := s.appsConfig()
	if cfg == nil {
		if req.Provider != "" || req.Features != nil || req.Restart != "" {
			return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "provider configuration is unavailable")
		}
		return nil, nil
	}
	owner := req.Provider
	if owner == "" {
		owner, _, _ = cfg.LookupVariant(req.Model)
	}
	registry := req.Registry
	if registry == "" {
		registry = constants.RepoOllama
		if strings.Contains(req.Model, "/") {
			registry = constants.RepoHuggingFace
		}
	}
	huggingFace := registry == constants.RepoHuggingFace || registry == constants.RepoHFAlias
	plans := make(map[string]deployPlan, len(nodes))
	listing, err := s.deployListing(ctx, cfg, req, owner, huggingFace)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		provider, err := s.deployProvider(ctx, req, owner, node, registry, listing.Architecture)
		if err != nil {
			return nil, err
		}
		svc, ok := cfg.LookupApp(provider)
		if !ok {
			return nil, invalidInputf("provider %q is not configured", provider)
		}
		names, err := selectedFeatures(svc, req.Features)
		if err != nil {
			return nil, invalidInputf("provider %q: %w", provider, err)
		}
		p := deployPlan{Provider: provider, Features: names}
		if huggingFace {
			d := featureDownload(req, svc, names)
			d.Files, err = huggingface.Plan(listing.Files, d)
			if err != nil {
				return nil, invalidInputf("%w", err)
			}
			p.Download = &d
		} else {
			for _, name := range names {
				f := svc.Features[name]
				if len(f.Files) > 0 || f.Runtime != "" {
					return nil, invalidInputf("provider %q feature %q is not built in", provider, name)
				}
			}
		}
		plans[node] = p
	}
	return plans, nil
}

func (s *DeploymentsService) deployListing(ctx context.Context, cfg *pkgConfig.AppsConfig, req *DeployRequest, owner string, huggingFace bool) (metadata.RepoFiles, error) {
	if !huggingFace {
		return metadata.RepoFiles{}, nil
	}
	if s.repoFiles == nil {
		return metadata.RepoFiles{}, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "registry file planner is unavailable")
	}
	svc, _ := cfg.LookupApp(owner)
	repo, _, _ := strings.Cut(svc.WeightsOf(req.Model), "#")
	listing, err := s.repoFiles(ctx, repo)
	if errors.Is(err, huggingface.ErrModelNotFound) {
		return listing, invalidInputf("%w", err)
	}
	return listing, err
}

func (s *DeploymentsService) deployProvider(ctx context.Context, req *DeployRequest, provider, node, registry, architecture string) (string, error) {
	if s.isCloudRegistry(registry) {
		if provider != "" && provider != registry {
			return "", invalidInputf("cloud registry %q requires provider %q", registry, registry)
		}
		return registry, nil
	}
	q := url.Values{"model": {req.Model}, "registry": {registry}, "format": {req.Format}, "architecture": {architecture}, "allow_unknown_architecture": {strconv.FormatBool(provider != "")}, "force": {strconv.FormatBool(req.Force)}}
	got, err := routeAndParse[struct {
		Data []deployEligibility `json:"data"`
	}](ctx, s.router, "GET", "/zzrouter/v1/internal/nodes/compatible?"+q.Encode(), node, nil)
	if err != nil {
		return "", fmt.Errorf("resolve provider on %s: %w", node, err)
	}
	var eligible, excluded []string
	for _, n := range got.Data {
		eligible = append(eligible, n.Providers...)
		excluded = append(excluded, n.ArchitectureExcluded...)
	}
	slices.Sort(eligible)
	eligible = slices.Compact(eligible)
	if (provider == "" && len(eligible) == 0 && len(excluded) > 0) || slices.Contains(excluded, provider) {
		hint := fmt.Sprintf("Call zz_model_download with model %q, node %q and a provider from eligible_providers", req.Model, node)
		if len(eligible) == 0 {
			hint = fmt.Sprintf("Call zz_providers with node %q to find a provider accepting this architecture before zz_provider_install and zz_model_download", node)
		}
		problem := newRouteProblemError(http.StatusBadRequest, "Bad Request", "provider is excluded by model architecture", httperr.RouteErrorCode(httperr.CodeArchitectureUnsupported), node)
		problem.ProblemDetails.Errors = []utils.ParamError{{
			Key: "provider", Code: string(httperr.CodeArchitectureUnsupported),
			Got:     map[string]any{"provider": provider, "architecture": architecture, "eligible_providers": eligible},
			Message: "choose an eligible provider for this architecture", Hint: hint,
		}}
		return "", problem
	}
	if provider == "" {
		if len(eligible) != 1 {
			return "", invalidInputf("node %q has %d eligible providers (%s); specify provider", node, len(eligible), strings.Join(eligible, ", "))
		}
		provider = eligible[0]
	} else if !slices.Contains(eligible, provider) {
		return "", invalidInputf("provider %q is not eligible on node %q", provider, node)
	}
	return provider, nil
}

func deployPlanKey(provider string, names []string, download *metadata.DownloadRequest) string {
	features := slices.Clone(names)
	slices.Sort(features)
	features = slices.Compact(features)
	var d metadata.DownloadRequest
	if download != nil {
		d = *download
		d.Files = slices.Clone(d.Files)
		slices.SortFunc(d.Files, func(a, b metadata.DownloadFile) int { return strings.Compare(a.Name, b.Name) })
		d.Want = slices.Clone(d.Want)
		d.Optional = slices.Clone(d.Optional)
		slices.Sort(d.Want)
		slices.Sort(d.Optional)
	}
	b, _ := json.Marshal(struct {
		Provider string
		Features []string
		Download metadata.DownloadRequest
	}{provider, features, d})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
