package process

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// ErrModelIncomplete is returned when required weights are absent or truncated.
var ErrModelIncomplete = layout.ErrIncomplete

// VerifyLocalWeights checks required weights and only manifest files read by this launch.
func VerifyLocalWeights(ctx context.Context, modelPath string, featurePaths []string) error {
	if modelPath == "" {
		return nil
	}
	modelPath = filepath.Clean(modelPath)
	complete, err := layout.Complete(modelPath)
	if err != nil {
		return fmt.Errorf("verify %s: %w", modelPath, err)
	}
	if !complete {
		return fmt.Errorf("%w: %s (re-download to repair)", ErrModelIncomplete, modelPath)
	}

	dir, ok := manifestDir(modelPath)
	if !ok {
		return nil
	}

	paths := append([]string{modelPath}, featurePaths...)
	info, statErr := os.Stat(modelPath)
	if layout.IsGGUF(modelPath) {
		variant, _ := layout.VariantOf(modelPath)
		for _, f := range variant.Files {
			paths = append(paths, filepath.Join(filepath.Dir(modelPath), filepath.FromSlash(f.Path)))
		}
	}
	complete, err = integrity.NewIntegrityVerifier().QuickVerify(ctx, dir, func(f integrity.FileChecksum) bool {
		p := filepath.Join(dir, f.RelativePath)
		return slices.Contains(paths, p) || (statErr == nil && info.IsDir() && f.Feature == "" && strings.HasPrefix(p, modelPath+string(filepath.Separator)))
	})
	if err != nil {
		return fmt.Errorf("verify %s: %w", dir, err)
	}
	if !complete {
		return fmt.Errorf("%w: %s (re-download to repair)", ErrModelIncomplete, dir)
	}
	return nil
}

// manifestDir returns the model directory holding the manifest that covers
// modelPath. llama.cpp is pointed at a file inside it, possibly in a quant
// subdirectory, so the search walks up from there, never past the models
// root: a manifest above it belongs to no model.
func manifestDir(modelPath string) (string, bool) {
	dir := modelPath
	if info, err := os.Stat(modelPath); err == nil && !info.IsDir() {
		dir = filepath.Dir(modelPath)
	}
	root, err := modelregistry.GetModelsRootDir()
	if err != nil {
		root = dir
	}
	for {
		if integrity.HasManifest(dir) {
			return dir, true
		}
		if rel, err := filepath.Rel(root, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return "", false
		}
		dir = filepath.Dir(dir)
	}
}
