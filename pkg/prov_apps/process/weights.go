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
func VerifyLocalWeights(ctx context.Context, modelPath string, featurePaths []string, sourcePaths ...string) error {
	if modelPath == "" {
		return nil
	}
	origin := modelPath
	if len(sourcePaths) > 0 && sourcePaths[0] != "" {
		origin = sourcePaths[0]
	}
	modelPath = layout.PhysicalPath(modelPath)
	complete, err := layout.Complete(modelPath)
	if err != nil {
		return fmt.Errorf("verify %s: %w", modelPath, err)
	}
	if !complete {
		return fmt.Errorf("%w: %s (re-download to repair)", ErrModelIncomplete, modelPath)
	}

	info, statErr := os.Stat(modelPath)
	sourceDir := filepath.Dir(origin)
	if statErr == nil && info.IsDir() {
		sourceDir = origin
	}
	dir, ok := manifestDir(sourceDir)
	if !ok {
		return nil
	}

	paths := append([]string{modelPath}, featurePaths...)
	if layout.IsGGUF(modelPath) {
		variant, _ := layout.VariantOf(origin)
		for _, f := range variant.Files {
			paths = append(paths, filepath.Join(filepath.Dir(origin), filepath.FromSlash(f.Path)))
		}
	}
	for i := range paths {
		paths[i] = layout.PhysicalPath(paths[i])
	}
	complete, err = integrity.NewIntegrityVerifier().QuickVerify(ctx, dir, func(f integrity.FileChecksum) bool {
		logical := filepath.Join(dir, f.RelativePath)
		return logical == filepath.Clean(origin) || slices.Contains(paths, layout.PhysicalPath(logical)) || (statErr == nil && info.IsDir() && f.Feature == "" && strings.HasPrefix(logical, filepath.Clean(origin)+string(filepath.Separator)))
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
func manifestDir(dir string) (string, bool) {
	root, err := modelregistry.GetModelsRootDir()
	if err != nil {
		root = dir
	}
	root = layout.PhysicalPath(root)
	for {
		if integrity.HasManifest(dir) {
			return dir, true
		}
		if rel, err := filepath.Rel(root, layout.PhysicalPath(dir)); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return "", false
		}
		dir = filepath.Dir(dir)
	}
}
