package modelregistry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/filesystem"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
)

// GetModel returns a specific model by path.
// For path-based lookups, checks filesystem first (faster than scanning
// all models); falls back to listing all models when path might be a
// model name rather than an absolute path.
func (r *Registry) GetModel(path string) (*metadata.ModelMetadata, error) {
	info, err := os.Stat(path)
	if err == nil {
		md := filesystem.AnalyzeModelFile(r.modelsRoot, path, info)
		if md != nil {
			return md, nil
		}
	}

	var found *metadata.ModelMetadata
	err = r.withModels(func(models []*metadata.ModelMetadata) error {
		for _, m := range models {
			if m.FullPath == path {
				found = m
				return nil
			}
		}
		return fmt.Errorf("model not found: %s", path)
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// AddModel is a no-op since models are discovered via filesystem
// scanning. No cache to invalidate — the server layer handles caching.
func (r *Registry) AddModel(md *metadata.ModelMetadata) error {
	_ = md
	return nil
}

// RemoveModel removes a model from the registry (alias for DeleteModel).
func (r *Registry) RemoveModel(path string) error {
	return r.DeleteModel(path)
}

// ValidateOllamaModel checks if an Ollama model exists in the library.
// Returns (false, nil) when Ollama is not configured — the connector
// treats "not configured" as "model doesn't exist here".
//
// The caller's ctx bounds the Ollama API call; a derived
// WithTimeout(ollama.APITimeout) protects against a stuck upstream.
func (r *Registry) ValidateOllamaModel(ctx context.Context, modelName string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, ollama.APITimeout)
	defer cancel()
	return r.ollamaConnector.ModelExists(cctx, modelName)
}

// ValidateHuggingFaceModel checks if a HuggingFace model exists.
// The caller's ctx bounds the HF API call; a derived
// WithTimeout(constants.HuggingFaceAPITimeout) protects against a
// stuck upstream.
func (r *Registry) ValidateHuggingFaceModel(ctx context.Context, modelID string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, constants.HuggingFaceAPITimeout)
	defer cancel()
	return r.huggingfaceConnector.ModelExists(cctx, modelID)
}

// DeletionTarget freezes physical files or the observed daemon for execution without lookup.
type DeletionTarget struct {
	Model     *metadata.ModelMetadata
	Paths     []string
	Backend   *backend.Resolved
	directory string
}

// SelectDeletion freezes a catalog entry's physical deletion identity.
func (r *Registry) SelectDeletion(model *metadata.ModelMetadata) (DeletionTarget, error) {
	target := DeletionTarget{Model: model}
	if model.SourceRepo == metadata.SourceOllama {
		resolved, ok := backend.NewResolver(r.appsConfigFn).Resolve(model.AssignedApp)
		if ok {
			resolved.Endpoint = config.NormalizeEndpoint(resolved.Endpoint)
		}
		if !ok || !backend.SameEndpoint(resolved.Endpoint, model.Backend) {
			return target, httperr.Error{Status: 503, Code: string(httperr.CodeClusterUnavailable), Message: "selected provider inventory no longer matches its backend; rescan the catalog"}
		}
		target.Backend = resolved
		return target, nil
	}
	if model.Backend != "" && model.FullPath == "" {
		return target, httperr.Error{Status: 409, Code: string(httperr.CodeModelNotDeletable), Message: "this external provider owns its model; remove it through the provider"}
	}
	target.directory = filepath.Dir(model.FullPath)
	if info, err := os.Stat(model.FullPath); err == nil && info.IsDir() {
		target.directory = model.FullPath
	}
	repo := filesystem.ExtractModelIDFromPath(r.modelsRoot, model.FullPath)
	if model.SourceRepo == metadata.SourceHuggingFace && model.SourceID != "" {
		repo = model.SourceID
	}
	if repo != "" {
		if !filepath.IsLocal(filepath.FromSlash(repo)) || filepath.Clean(repo) == "." {
			return target, fmt.Errorf("repository %q is outside the models root", repo)
		}
		target.directory = string(r.CatalogDestination(repo))
	}
	target.Paths = []string{model.FullPath}
	if model.Format == metadata.FormatGGUF && !model.Incomplete {
		if variant, ok := layout.VariantOf(model.FullPath); ok {
			target.Paths = nil
			for _, file := range variant.Files {
				target.Paths = append(target.Paths, filepath.Join(filepath.Dir(model.FullPath), filepath.FromSlash(file.Path)))
			}
		}
	}
	return target, nil
}

// DeleteSelected executes exactly the identity selected under deletion ownership.
func (r *Registry) DeleteSelected(ctx context.Context, target DeletionTarget) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if target.Backend != nil {
		err := r.ollamaConnector.DeleteModel(ctx, target.Model.Name, target.Backend)
		var refusal httperr.Error
		knownRefusal := errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500
		return !knownRefusal, err // transport and 5xx outcomes may have applied the deletion
	}
	release, err := integrity.TryAcquireMaterialization(ctx, target.directory)
	if errors.Is(err, integrity.ErrMaterializationHeld) {
		err = httperr.Error{Status: 409, Code: string(httperr.CodeDownloadInProgress), Message: "model files are being written; cancel the job and wait for its terminal outcome before deleting"}
	}
	if err != nil {
		return false, err
	}
	defer release()
	changed := false
	for _, path := range target.Paths {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		if err := r.DeleteModel(path); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// DeleteModelByNameAndSource keeps direct registry callers on exact selected execution.
func (r *Registry) DeleteModelByNameAndSource(ctx context.Context, modelName, source string) error {
	models, failures, err := r.ModelSnapshot(ctx)
	if err != nil {
		return err
	}
	var selected *metadata.ModelMetadata
	for _, model := range models {
		if strings.EqualFold(model.Name, modelName) && (source == "" || strings.EqualFold(model.SourceRepo, source)) {
			if selected != nil {
				return httperr.Error{Status: 409, Code: string(httperr.CodeModelSourceAmbiguous), Message: "model has several physical sources; select exact catalog identity"}
			}
			selected = model
		}
	}
	if selected == nil {
		if len(failures) > 0 {
			return httperr.Error{Status: 503, Message: "model inventory is unavailable"}
		}
		return httperr.Error{Status: 404, Code: string(httperr.CodeUnknownModel), Message: "model is absent from the local catalog"}
	}
	target, err := r.SelectDeletion(selected)
	if err != nil {
		return err
	}
	_, err = r.DeleteSelected(ctx, target)
	return err
}
