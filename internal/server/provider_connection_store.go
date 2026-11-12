package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
)

func openProviderConnectionStore(cfg config.CredentialsConfig, dir string) (*secrets.Store, error) {
	path := filepath.Join(dir, "credentials.enc")
	if cfg.KeyFile == "" {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("existing protected connections require security.credentials.key_file")
		}
		return nil, nil
	}
	keyPath := cfg.KeyFile
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(dir, keyPath)
	}
	key, err := secrets.LoadKeyFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("load provider connection protection: %w", err)
	}
	defer clear(key)
	store, err := secrets.Open(path, key)
	if err != nil {
		return nil, fmt.Errorf("open protected provider connections: %w", err)
	}
	return store, nil
}

func (s *Server) connectionChanged(name string) {
	if s.providers.appMgr != nil {
		version := "unknown"
		if s.backend.Available(name) {
			version = "cloud"
		}
		s.providers.appMgr.SetProviderVersion(name, version)
	}
	if s.model == nil {
		return
	}
	if s.model.Registry != nil {
		s.model.Registry.InvalidateScanCache()
	}
	if s.model.Cache != nil {
		s.model.Cache.Invalidate()
	}
}
