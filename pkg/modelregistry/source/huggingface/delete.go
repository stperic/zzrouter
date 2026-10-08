package huggingface

import (
	"os"
	"path/filepath"
)

// DeleteModel deletes a HuggingFace model directory from the filesystem
// and recursively prunes any now-empty parent directories up to the
// models root — so removing "org-name/model-v1" also takes out the
// "org-name" container when it was the last entry.
func (hfc *Connector) DeleteModel(modelName string) error {
	modelPath := filepath.Join(hfc.modelsDir, modelName)

	if err := os.RemoveAll(modelPath); err != nil {
		return err
	}

	hfc.cleanupEmptyParents(filepath.Dir(modelPath))
	return nil
}

// cleanupEmptyParents recursively removes empty parent directories up to
// modelsDir. Bails out on any non-empty dir or I/O error — never touches
// the models root itself.
func (hfc *Connector) cleanupEmptyParents(dir string) {
	if dir == hfc.modelsDir || dir == "" {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}

	if err := os.Remove(dir); err != nil {
		return
	}

	hfc.cleanupEmptyParents(filepath.Dir(dir))
}
