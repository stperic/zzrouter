package server

import (
	"context"

	"github.com/stperic/zzrouter/pkg/constants"
)

// cleanupConstruction releases owners acquired before the constructor failed.
// Start has not run, but constructors such as jobs.NewRegistry own goroutines.
func (s *Server) cleanupConstruction() {
	if s.shutdownCancel != nil {
		s.shutdownCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), constants.ShutdownSubsystemTimeout)
	defer cancel()
	s.services.Deployments.Stop(ctx)
	s.services.Search.Stop(ctx)
	s.services.Discovery.Stop()
	_ = s.providers.appMgr.Stop(ctx)
	s.providers.cooldowns.Stop()
	if s.model != nil {
		s.model.Pricing.Stop()
		s.model.Downloads.Stop()
		if s.model.Registry != nil {
			s.model.Registry.Stop()
		}
		if s.model.Cache != nil {
			s.model.Cache.Stop(ctx)
		}
	}
	if s.jobs != nil {
		s.jobs.Stop()
	}
	s.access.Stop(ctx)
	if s.access != nil && s.access.oauth != nil {
		_ = s.access.oauth.Close()
	}
	if s.providers.connectionStore != nil {
		_ = s.providers.connectionStore.Close()
	}
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	if s.httpStreamingClient != nil {
		s.httpStreamingClient.CloseIdleConnections()
	}
}
