package server

import (
	"context"
	"net/http"
	"net/url"
	"path"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	provapps "github.com/stperic/zzrouter/pkg/prov_apps"
)

type providerModelList struct {
	Provider   string                        `json:"provider"`
	Node       string                        `json:"node"`
	Categories []provapps.ModelStoreCategory `json:"categories"`
}

type providerModelDelete struct {
	Provider string `json:"provider"`
	Node     string `json:"node"`
	Category string `json:"category"`
	Name     string `json:"name"`
	Deleted  bool   `json:"deleted"`
}

type providerModelDownloadRequest struct {
	Node     string `json:"node" binding:"required"`
	Category string `json:"category" binding:"required"`
	Source   string `json:"source" binding:"required"`
	Name     string `json:"name,omitempty"`
}

type providerModelDownload struct {
	Provider string `json:"provider"`
	Node     string `json:"node"`
	Category string `json:"category"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
}

func providerModelsPath(provider, category, name string) string {
	query := url.Values{"category": {category}, "name": {name}}
	return "/zzrouter/v1/internal/providers/" + url.PathEscape(provider) + "/models?" + query.Encode()
}

// ListProviderModels reads exactly the named node's provider filesystem.
func (s *ProvidersService) ListProviderModels(ctx context.Context, provider, node, category string) (*providerModelList, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	return routeAndParse[providerModelList](ctx, s.router, "GET", providerModelsPath(provider, category, ""), node, nil)
}

// DeleteProviderModel routes one regular-file unlink to the named node.
func (s *ProvidersService) DeleteProviderModel(ctx context.Context, provider, node, category, name string) (*providerModelDelete, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	return routeAndParse[providerModelDelete](ctx, s.router, "DELETE", providerModelsPath(provider, category, name), node, nil)
}

// DownloadProviderModel uses the shared download dispatch, job and admission.
func (s *ProvidersService) DownloadProviderModel(ctx context.Context, provider string, req providerModelDownloadRequest) (*providerModelDownload, error) {
	ctx, cancel := ensureTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	download, err := huggingface.ParseSource(req.Source)
	if err != nil {
		return nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeInvalidValue), Message: err.Error()}
	}
	if req.Name == "" {
		req.Name = path.Base(download.Weights)
	}
	download.Target = metadata.DownloadTarget{Owner: metadata.OwnerProviderStore, Provider: provider, Category: req.Category, Name: req.Name}
	result, err := dispatchDownload(ctx, s.router, req.Node, internalDownloadRequest{Model: download.Repo, Registry: constants.RepoHuggingFace, Download: &download})
	if err != nil {
		return nil, err
	}
	return &providerModelDownload{Provider: provider, Node: result.Node, Category: req.Category, Name: req.Name,
		Source: req.Source, Revision: download.EffectiveRevision(), Status: result.Status, JobID: result.JobID}, nil
}
