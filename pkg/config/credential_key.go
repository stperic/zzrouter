package config

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/security"
)

// CredentialKeyPath resolves the protection key without reading or creating it.
func CredentialKeyPath(dir string, cfg CredentialsConfig) string {
	if cfg.KeyFile == "" {
		return filepath.Join(dir, "private", "credentials.key")
	}
	if filepath.IsAbs(cfg.KeyFile) {
		return cfg.KeyFile
	}
	return filepath.Join(dir, cfg.KeyFile)
}

// PrepareCredentialKey runs as the node account before opening protected
// connections. Explicit keys are read-only; the automatic key is created once.
// The caller must clear the returned bytes after opening the encrypted store.
func PrepareCredentialKey(ctx context.Context, dir string, cfg CredentialsConfig) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := CredentialKeyPath(dir, cfg)
	if cfg.KeyFile != "" {
		return secrets.LoadKeyFile(path)
	}
	private := filepath.Dir(path)
	if err := os.MkdirAll(private, 0o700); err != nil {
		return nil, fmt.Errorf("create credential key directory: %w", err)
	}
	info, err := os.Lstat(private)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("credential key directory must be a real directory: %s", private)
	}
	if err := security.RestrictDirectoryPermissions(private); err != nil {
		return nil, err
	}
	lock, err := atomicfile.Lock(filepath.Join(private, "credentials.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock credential key preparation: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("automatic credential key must be a regular file")
		}
		return secrets.LoadKeyFile(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect credential key: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "credentials.enc")); err == nil {
		return nil, errors.New("protected connections exist without their key; restore the matching protection key or set security.credentials.key_file")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect protected connections: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate credential protection key: %w", err)
	}
	if err := atomicfile.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("save credential protection key: %w", err)
	}
	return secrets.LoadKeyFile(path)
}
