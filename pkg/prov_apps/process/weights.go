package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/model/integrity"
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
func VerifyLocalWeights(ctx context.Context, modelPath string) error {
	if modelPath == "" {
		return nil
	}

	dir, ok := manifestDir(modelPath)
	if !ok {
		return nil
	}

	complete, err := integrity.NewIntegrityVerifier().QuickVerify(ctx, dir)
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
