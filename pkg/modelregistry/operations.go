package modelregistry

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// DeleteModel removes a model file/directory
// Models are discovered via filesystem scanning, so we just delete the file
func (r *Registry) DeleteModel(path string) error {
	// Verify file/directory exists
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("model not found: %s", path)
	}

	// Delete the actual file/directory
	if err := deleteModelFile(path); err != nil {
		return fmt.Errorf("failed to delete model file: %w", err)
	}

	return nil
}

// DeleteModels deletes multiple models in batch
// Returns count of successfully deleted models and any errors encountered
func (r *Registry) DeleteModels(paths []string) (int, []error) {
	deleted := 0
	errors := make([]error, 0)

	for _, path := range paths {
		if err := r.DeleteModel(path); err != nil {
			errors = append(errors, fmt.Errorf("%s: %w", path, err))
		} else {
			deleted++
		}
	}

	return deleted, errors
}

// VerifyModel verifies a model's integrity by checking its checksum
func (r *Registry) VerifyModel(path string) error {
	// Check if file exists
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("model file missing: %w", err)
	}

	// For now, just verify file exists
	// Checksum verification would require storing checksums somewhere
	// which we're not doing anymore without the index
	return nil
}

// VerifyAllModels verifies all models in the registry
// Returns lists of valid and invalid models
func (r *Registry) VerifyAllModels() (valid, invalid []*metadata.ModelMetadata, errors []error) {
	valid = make([]*metadata.ModelMetadata, 0)
	invalid = make([]*metadata.ModelMetadata, 0)
	errors = make([]error, 0)

	err := r.withModels(func(models []*metadata.ModelMetadata) error {
		for _, model := range models {
			if err := r.VerifyModel(model.FullPath); err != nil {
				invalid = append(invalid, model)
				errors = append(errors, fmt.Errorf("%s: %w", model.Name, err))
			} else {
				valid = append(valid, model)
			}
		}
		return nil
	})

	if err != nil {
		errors = append(errors, fmt.Errorf("failed to list models: %w", err))
	}

	return valid, invalid, errors
}

// deleteModelFile deletes a model file or directory
func deleteModelFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	// Store parent directory before deletion
	parentDir := filepath.Dir(path)

	if info.IsDir() {
		// Delete directory and all contents
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	} else {
		// Delete single file
		if err := os.Remove(path); err != nil {
			return err
		}
	}

	// Clean up empty parent directories (for both files and directories)
	cleanupEmptyDirs(parentDir)

	return nil
}

// cleanupEmptyDirs removes empty parent directories up to the models root
func cleanupEmptyDirs(dir string) {
	modelsRoot, err := GetModelsRootDir()
	if err != nil {
		return
	}

	// Don't delete the models root itself
	if dir == modelsRoot {
		return
	}

	// Check if directory is empty
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 0 {
		return
	}

	// Directory is empty, remove it
	if err := os.Remove(dir); err != nil {
		return
	}

	// Recursively clean parent
	cleanupEmptyDirs(filepath.Dir(dir))
}

// GetTotalDiskUsage returns the total disk space used by all models
func (r *Registry) GetTotalDiskUsage() int64 {
	var total int64
	_ = r.withModels(func(models []*metadata.ModelMetadata) error {
		for _, model := range models {
			total += model.Size
		}
		return nil
	})
	return total
}

// GetLargestModels returns the N largest models by size
func (r *Registry) GetLargestModels(n int) []*metadata.ModelMetadata {
	models, err := r.ListAllModels()
	if err != nil {
		return nil
	}

	if len(models) == 0 {
		return []*metadata.ModelMetadata{}
	}

	// Create a copy to avoid modifying the cached slice
	sorted := make([]*metadata.ModelMetadata, len(models))
	copy(sorted, models)

	// Sort by size (descending) using efficient sort
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Size > sorted[j].Size
	})

	// Return top N
	if n > len(sorted) {
		n = len(sorted)
	}
	if n <= 0 {
		return []*metadata.ModelMetadata{}
	}
	return sorted[:n]
}
