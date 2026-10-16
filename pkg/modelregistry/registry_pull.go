package modelregistry

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/huggingface"
	"github.com/stperic/zzrouter/pkg/modelregistry/source/ollama"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ListHuggingFaceRepoFiles returns the file list of a HuggingFace repo
// (one network call). Surfaced on Registry so HTTP-layer callers don't
// need to reach through to the connector type.
func (r *Registry) ListHuggingFaceRepoFiles(ctx context.Context, modelID string) ([]metadata.TreeFileEntry, error) {
	return r.huggingfaceConnector.GetRepoFiles(ctx, modelID)
}

// PullOllamaModel pulls a model from Ollama in the caller's goroutine.
// The provided ctx is used for the HTTP request — cancelling it aborts
// the download.
//
// The IsConfigured pre-check is a fast-path: PullModelWithContext would
// itself return ollama.ErrNotConfigured on an inactive connector, but
// by then we'd have fired a "downloading" update. Refuse before side effects.
func (r *Registry) PullOllamaModel(ctx context.Context, modelName string, statusUpdater func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64)) {
	if !r.ollamaConnector.IsConfigured() {
		statusUpdater("failed", ollama.ErrNotConfigured.Error(), 0, 0, 0, 0, 0, 0)
		return
	}

	if !r.beginDownload() {
		statusUpdater("failed", ErrRegistryStopped.Error(), 0, 0, 0, 0, 0, 0)
		return
	}

	defer r.wg.Done()
	slog.Info("Ollama deploy started", "model", modelName)
	statusUpdater("downloading", "", 0, 0, 0, 0, 0, 0)

	var totalBytes int64
	var completedBytes int64
	startTime := utils.Now()

	err := r.ollamaConnector.PullModelWithContext(ctx, modelName, func(status, digest string, total, completed int64) {
		totalBytes = total
		completedBytes = completed

		progress := float64(0)
		if total > 0 {
			progress = float64(completed) / float64(total) * 100
		}

		// Calculate speed
		elapsed := time.Since(startTime).Seconds()
		speed := int64(0)
		if elapsed > 0 {
			speed = int64(float64(completed) / elapsed)
		}
		// Format a user-friendly message from Ollama's status
		message := status
		if digest != "" {
			message = fmt.Sprintf("%s %s", status, digest)
		}
		statusUpdater("downloading", message, 0, 0, progress, completedBytes, totalBytes, speed)
	})

	if err != nil {
		// Don't log context cancellation as an error — it's an intentional stop
		if ctx.Err() != nil {
			slog.Info("Ollama deploy cancelled", "model", modelName)
			statusUpdater(string(constants.StatusCancelled), "", 0, 0, 0, completedBytes, totalBytes, 0)
		} else {
			slog.Error("Ollama deploy failed", "model", modelName, "error", err)
			statusUpdater("failed", err.Error(), 0, 0, 0, completedBytes, totalBytes, 0)
		}
	} else if totalBytes == 0 {
		// Ollama returned success but downloaded nothing — model likely doesn't exist
		slog.Warn("Ollama deploy returned success but downloaded 0 bytes: model may not exist in Ollama registry", "model", modelName)
		statusUpdater("failed", "model not found in Ollama registry (0 bytes downloaded)", 0, 0, 0, 0, 0, 0)
	} else {
		slog.Info("Ollama deploy completed", "model", modelName, "bytes", totalBytes)
		statusUpdater("completed", "", 0, 0, 100, totalBytes, totalBytes, 0)
	}
}

// DownloadHuggingFaceModel fetches what req names from HuggingFace in
// the caller's goroutine. Cancelling ctx aborts it. Progress and completion go
// to statusUpdater; failures return after physical transfer and cleanup finish.
func (r *Registry) DownloadHuggingFaceModel(ctx context.Context, req metadata.DownloadRequest, statusUpdater func(status, currentFile string, filesDownloaded, filesTotal int, progress float64, downloaded, totalSize, speed int64), destination huggingface.Destination) error {
	if !r.beginDownload() {
		return ErrRegistryStopped
	}

	defer r.wg.Done()
	startTime := utils.Now()

	err := r.huggingfaceConnector.Download(ctx, req, func(status string, bytesDownloaded, totalBytes int64) {
		progress := float64(0)
		if totalBytes > 0 {
			progress = float64(bytesDownloaded) / float64(totalBytes) * 100
		}
		speed := int64(0)
		if elapsed := utils.Now().Sub(startTime).Seconds(); elapsed > 0 {
			speed = int64(float64(bytesDownloaded) / elapsed)
		}
		statusUpdater(status, req.Weights, 0, 1, progress, bytesDownloaded, totalBytes, speed)
	}, destination)

	if err != nil {
		if ctx.Err() != nil {
			slog.Info("HuggingFace download cancelled", "model", req.Repo)
		} else {
			slog.Error("HuggingFace download failed", "model", req.Repo, "error", err)
		}
		return err
	}
	// The admitted owner publishes before its tracker accepts completion.
	statusUpdater("completed", req.Weights, 1, 1, 100, 0, 0, 0)
	return nil
}

// CatalogDestination resolves a repository in this registry's bound model root.
func (r *Registry) CatalogDestination(repo string) huggingface.CatalogDestination {
	return huggingface.CatalogDestination(filepath.Join(r.modelsRoot, filepath.FromSlash(repo)))
}
