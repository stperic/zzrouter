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
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
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
	registry := req.Registry
	if registry == "" {
		registry = constants.RepoOllama
		if strings.Contains(req.Model, "/") {
			registry = constants.RepoHuggingFace
		}
	}
	plans := make(map[string]deployPlan, len(nodes))
	var listing []metadata.TreeFileEntry
	listed := false
	for _, node := range nodes {
		provider := req.Provider
		if s.isCloudRegistry(registry) {
			if provider != "" && provider != registry {
				return nil, invalidInputf("cloud registry %q requires provider %q", registry, registry)
			}
			provider = registry
		} else {
			q := url.Values{"model": {req.Model}, "registry": {registry}, "format": {req.Format}, "force": {strconv.FormatBool(req.Force)}}
			got, err := routeAndParse[struct {
				Data []struct {
					Providers []string `json:"eligible_providers"`
				} `json:"data"`
			}](ctx, s.router, "GET", "/zzrouter/v1/internal/nodes/compatible?"+q.Encode(), node, nil)
			if err != nil {
				return nil, fmt.Errorf("resolve provider on %s: %w", node, err)
			}
			var eligible []string
			for _, n := range got.Data {
				eligible = append(eligible, n.Providers...)
			}
			slices.Sort(eligible)
			eligible = slices.Compact(eligible)
			if provider == "" {
				if len(eligible) != 1 {
					return nil, invalidInputf("node %q has %d eligible providers (%s); specify provider", node, len(eligible), strings.Join(eligible, ", "))
				}
				provider = eligible[0]
			} else if !slices.Contains(eligible, provider) {
				return nil, invalidInputf("provider %q is not eligible on node %q", provider, node)
			}
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
		if registry == constants.RepoHuggingFace || registry == constants.RepoHFAlias {
			if s.repoFiles == nil {
				return nil, newProblemError(http.StatusServiceUnavailable, "Service Unavailable", "registry file planner is unavailable")
			}
			d := featureDownload(req, svc, names)
			if !listed {
				listing, err = s.repoFiles(ctx, d.Repo)
				if errors.Is(err, huggingface.ErrModelNotFound) {
					return nil, invalidInputf("%w", err)
				}
				if err != nil {
					return nil, err
				}
				listed = true
			}
			d.Files, err = huggingface.Plan(listing, d)
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
