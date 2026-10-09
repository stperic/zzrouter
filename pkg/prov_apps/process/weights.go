package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/model/layout"
	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// ErrModelIncomplete is returned when a model's files don't match the
// integrity manifest written at download time — the signature of an
// interrupted or truncated download.
var ErrModelIncomplete = errors.New("model weights incomplete")

// VerifyLocalWeights checks a resolved model path against the integrity
// manifest zzRouter wrote when it downloaded the model.
//
// Only presence and size are checked, never content hashes: this runs on
// the launch path, and re-hashing tens of gigabytes to answer "did the
// download finish" would cost more than the launch itself. Models with no
// manifest (Ollama pulls, hand-placed files) pass through — the manifest is
// evidence when present, not a requirement.
func VerifyLocalWeights(ctx context.Context, modelPath string, featurePaths ...string) error {
	if modelPath == "" {
		return nil
	}
	modelPath = filepath.Clean(modelPath)

	dir, ok := manifestDir(modelPath)
	if !ok {
		return nil
	}

	paths := append([]string{modelPath}, featurePaths...)
	info, statErr := os.Stat(modelPath)
	if layout.IsGGUF(modelPath) {
		variant, ok := layout.VariantOf(modelPath)
		if !ok {
			return fmt.Errorf("%w: %s (re-download to repair)", ErrModelIncomplete, modelPath)
		}
		for _, f := range variant.Files {
			paths = append(paths, filepath.Join(filepath.Dir(modelPath), filepath.FromSlash(f.Path)))
		}
	}
	complete, err := integrity.NewIntegrityVerifier().QuickVerify(ctx, dir, func(f integrity.FileChecksum) bool {
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
