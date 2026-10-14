package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ResolvePath resolves existing symlinks, preserving a not-yet-created suffix.
func ResolvePath(dir string) (string, error) {
	key, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	parent := key
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			key = resolved
			for i := len(suffix) - 1; i >= 0; i-- {
				key = filepath.Join(key, suffix[i])
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		suffix = append(suffix, filepath.Base(parent))
		parent = next
	}
	return key, nil
}

// RequireAbsent refuses every existing final entry, including symlinks.
func RequireAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		return fs.ErrExist
	}
	return err
}
