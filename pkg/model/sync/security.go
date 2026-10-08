package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/modelregistry"
)

// Security provides path-validation and containment checks for model sync
// operations. All accepted paths must resolve (after symlink evaluation)
// inside the configured models root; escape attempts are rejected.
type Security struct {
	modelsRoot   string // as-configured, used for error messages.
	resolvedRoot string // absolute + EvalSymlinks'd, used for containment math.
}

// NewSecurity creates a Security instance for the default models root
// (from pkg/modelregistry). On failure to resolve the root, it falls
// back to /var/lib/zzrouter/models.
func NewSecurity() *Security {
	modelsRoot, err := modelregistry.GetModelsRootDir()
	if err != nil {
		modelsRoot = "/var/lib/zzrouter/models"
	}
	return NewSecurityFromRoot(modelsRoot)
}

// NewSecurityFromRoot constructs a Security for an explicit root.
// Exported so tests can pass a tempdir. EvalSymlinks may fail on a fresh
// install where the dir doesn't exist yet; in that case resolvedRoot
// falls back to the Abs value.
func NewSecurityFromRoot(root string) *Security {
	resolved, _ := filepath.Abs(root)
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	return &Security{modelsRoot: root, resolvedRoot: resolved}
}

// ValidateModelPath validates that a requested path is safe and resolves
// to a file inside modelsRoot, following symlinks. Returns the full path.
func (s *Security) ValidateModelPath(requestedPath string) (string, error) {
	if requestedPath == "" {
		return "", fmt.Errorf("path is required")
	}

	cleanPath := filepath.Clean(requestedPath)
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	cleanPath = strings.TrimPrefix(cleanPath, ".")
	if strings.Contains(cleanPath, "..") {
		return "", fmt.Errorf("invalid path: path traversal detected")
	}

	fullPath := filepath.Join(s.resolvedRoot, cleanPath)
	if !isWithin(s.resolvedRoot, fullPath) {
		return "", fmt.Errorf("invalid path: outside models directory")
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path not found %q: %w", cleanPath, err)
		}
		return "", fmt.Errorf("failed to stat path: %w", err)
	}

	// Resolve symlinks on the target and re-check containment: an attacker
	// who can place a symlink inside modelsRoot would otherwise read any
	// file on the host.
	resolved, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve symlinks: %w", err)
	}
	if !isWithin(s.resolvedRoot, resolved) {
		return "", fmt.Errorf("invalid path: symlink target escapes models directory")
	}

	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid path: not a regular file")
	}

	return fullPath, nil
}

// ValidateModelDir validates that a path is a valid model directory.
func (s *Security) ValidateModelDir(requestedPath string) (string, error) {
	fullPath, err := s.ValidateModelPath(requestedPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		return "", err
	}

	if !info.IsDir() {
		return "", fmt.Errorf("invalid path: not a directory")
	}

	return fullPath, nil
}

// ValidateWritePath checks containment through existing ancestors before
// creating directories or replacing files in a sync destination.
func (s *Security) ValidateWritePath(requestedPath string) (string, error) {
	if requestedPath == "" || filepath.IsAbs(requestedPath) || strings.Contains(requestedPath, "..") {
		return "", fmt.Errorf("invalid destination path")
	}
	full := filepath.Join(s.resolvedRoot, requestedPath)
	if !isWithin(s.resolvedRoot, full) {
		return "", fmt.Errorf("destination outside models directory")
	}
	ancestor := full
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if ancestor == s.resolvedRoot {
			return "", err
		}
		ancestor = filepath.Dir(ancestor)
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	if !isWithin(s.resolvedRoot, resolved) {
		return "", fmt.Errorf("destination symlink escapes models directory")
	}
	return full, nil
}

// ValidateModelFile validates that a path is a valid model file.
func (s *Security) ValidateModelFile(requestedPath string) (string, error) {
	fullPath, err := s.ValidateModelPath(requestedPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		return "", err
	}

	if info.IsDir() {
		return "", fmt.Errorf("invalid path: is a directory, expected file")
	}

	return fullPath, nil
}

// isWithin reports whether target is inside or equal to root. Uses
// filepath.Rel so sibling paths that share a string prefix
// ("<root>-evil") are correctly excluded.
func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
