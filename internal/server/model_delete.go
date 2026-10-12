package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
)

// HandleInternalDeleteModels owns local selection, refusal, mutation and publication.
func (e *InternalExecutor) HandleInternalDeleteModels(c *gin.Context) {
	var req DeleteModelsRequest
	if !BindJSONStrict(c, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		BadRequest(c, err.Error())
		return
	}
	response, err := e.deleteLocal(c.Request.Context(), &req)
	if err != nil {
		RespondToError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func deletionFailure(err error, completed int) error {
	return httperr.Error{Status: http.StatusServiceUnavailable, Code: string(httperr.CodeClusterUnavailable), Message: fmt.Sprintf("deletion completed %d models, but remaining deletion, publication or configuration failed: %v; inspect the catalog before a new operation", completed, err), Extra: map[string]any{"deleted": completed, "mutation_applied": true}}
}

func (e *InternalExecutor) deleteLocal(ctx context.Context, req *DeleteModelsRequest) (*DeleteModelsResponse, error) {
	response := &DeleteModelsResponse{Model: req.Model, Node: e.getNodename()}
	if e.registry == nil || e.deletionManager == nil || e.modelCache == nil || e.publishCatalog == nil {
		return response, newProblemError(503, "Service Unavailable", "model deletion ownership is unavailable")
	}
	cloud := map[string][]string{}
	err := e.deletionManager.WithModelDeletion(ctx, func(ctx context.Context) error {
		models, failures, err := e.registry.ModelSnapshot(ctx)
		if err != nil {
			return err
		}
		if req.Model != "" && failures[req.Provider] != "" {
			return newProblemError(503, "Service Unavailable", "selected model inventory is unavailable")
		}
		catalog, err := e.modelCache.ListModels(ctx, e.getNodename(), "", "", "")
		if err != nil {
			return err
		}
		for _, entry := range catalog {
			if entry.IsCloud {
				models = append(models, &metadata.ModelMetadata{Name: entry.Name, SourceRepo: entry.SourceRepo, AssignedApp: entry.Provider, SourceID: entry.SourceID})
			}
		}
		targets, err := e.deletionTargets(req, models, response)
		if err != nil {
			return err
		}
		changed := false
		for _, target := range targets {
			model := target.Model
			if e.isCloudProvider(model.SourceRepo) {
				cloud[model.SourceRepo] = append(cloud[model.SourceRepo], model.Name)
				continue
			}
			identity := instance.Weights{Model: model.Name, Source: model.SourceRepo, Paths: target.Paths}
			if target.Backend != nil {
				identity.Backend = target.Backend.Endpoint
			}
			if err := e.deletionManager.CheckModelDeletion(ctx, identity); err != nil {
				if req.Model != "" {
					return err
				}
				response.Errors = append(response.Errors, err.Error())
				continue
			}
			mutated, err := e.registry.DeleteSelected(ctx, target)
			changed = changed || mutated
			if err == nil {
				response.Deleted++
			} else if req.Model != "" {
				if changed {
					if publishErr := e.publishCatalog(ctx); publishErr != nil {
						return deletionFailure(publishErr, response.Deleted)
					}
					return deletionFailure(err, response.Deleted)
				}
				return err
			} else {
				response.Errors = append(response.Errors, fmt.Sprintf("%s: %v", model.Name, err))
			}
		}
		if changed {
			if err := e.publishCatalog(ctx); err != nil {
				return deletionFailure(err, response.Deleted)
			}
		}
		return nil
	})
	if err != nil {
		return response, err
	}
	if err := e.persistCloudDeletion(ctx, req.Model != "", cloud, response); err != nil {
		return response, err
	}

	return response, nil
}

func (e *InternalExecutor) deletionTargets(req *DeleteModelsRequest, models []*metadata.ModelMetadata, response *DeleteModelsResponse) ([]modelregistry.DeletionTarget, error) {
	var targets []modelregistry.DeletionTarget
	selectModel := func(name, source, provider, id string, exact bool) error {
		var matches []*metadata.ModelMetadata
		for _, model := range models {
			matched := strings.EqualFold(name, model.Name)
			if !exact {
				matched = modelregistry.MatchPattern(model.Name, name)
			}
			if matched && (source == "" || strings.EqualFold(source, model.SourceRepo)) && (provider == "" || strings.EqualFold(provider, model.AssignedApp)) && (id == "" || strings.EqualFold(id, model.SourceID)) {
				matches = append(matches, model)
			}
		}
		if len(matches) == 0 {
			return httperr.Error{Status: 404, Code: string(httperr.CodeUnknownModel), Message: "model is absent from the local catalog"}
		}
		if exact && len(matches) > 1 {
			return httperr.Error{Status: 409, Code: string(httperr.CodeModelSourceAmbiguous), Message: "model has several physical sources; use provider and registry selectors"}
		}
		for _, model := range matches {
			if e.isCloudProvider(model.SourceRepo) {
				if req.Model != "" {
					return httperr.Error{Status: 409, Code: string(httperr.CodeModelNotDeletable), Message: "cloud models are configuration; remove them through the admin models batch API"}
				}
				targets = append(targets, modelregistry.DeletionTarget{Model: model})
				continue
			}
			target, err := e.registry.SelectDeletion(model)
			if err != nil {
				if exact {
					return err
				}
				response.Errors = append(response.Errors, fmt.Sprintf("%s: %v", model.Name, err))
				continue
			}
			targets = append(targets, target)
		}
		return nil
	}
	if req.Model != "" {
		err := selectModel(req.Model, req.Registry, req.Provider, req.SourceID, true)
		return targets, err
	}
	for _, model := range req.Models {
		if err := selectModel(model.Name, model.Registry, model.Provider, model.SourceID, true); err != nil {
			response.Errors = append(response.Errors, fmt.Sprintf("%s: %v", model.Name, err))
		}
	}
	if req.Pattern != "" {
		if err := selectModel(req.Pattern, "", "", "", false); err != nil {
			response.Errors = append(response.Errors, err.Error())
		}
	}
	for _, path := range req.Paths {
		model := &metadata.ModelMetadata{Name: filepath.Base(path), FullPath: path}
		for _, observed := range models {
			if observed.FullPath == path {
				model = observed
				break
			}
		}
		target, err := e.registry.SelectDeletion(model)
		if err != nil {
			response.Errors = append(response.Errors, err.Error())
		} else {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

// persistCloudDeletion runs configuration reloads outside physical deletion ownership.
func (e *InternalExecutor) persistCloudDeletion(ctx context.Context, exact bool, cloud map[string][]string, response *DeleteModelsResponse) error {
	if len(cloud) > 0 {
		if err := ctx.Err(); err != nil {
			return deletionFailure(err, response.Deleted)
		}
		if err := e.configStore.RemoveCloudModels(cloud); err != nil {
			if exact {
				return deletionFailure(err, response.Deleted)
			}
			response.Errors = append(response.Errors, "cloud model removal: "+err.Error())
		} else {
			for _, names := range cloud {
				response.Deleted += len(names)
			}
			if err := e.publishCatalog(ctx); err != nil {
				return deletionFailure(err, response.Deleted)
			}
		}
	}
	return nil
}
