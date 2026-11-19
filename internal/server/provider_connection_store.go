package server

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/stperic/common/secrets"
	"github.com/stperic/zzrouter/pkg/config"
)

func openProviderConnectionStore(cfg config.CredentialsConfig, dir string) (*secrets.Store, error) {
	path := filepath.Join(dir, "credentials.enc")
	key, err := config.PrepareCredentialKey(context.Background(), dir, cfg)
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
		s.providers.appMgr.RefreshProviderVersion(s.shutdownCtx, name)
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
