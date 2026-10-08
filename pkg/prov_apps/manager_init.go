package prov_apps

import (
	"context"
	"sync"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// DiscoverAndRegister seeds the version cache for enabled providers.
//
// The generic discovery subsystem (runtime python imports, executable
// version parsing, HTTP version probes beyond the one Ollama ping) was
// cut — managed installs cache their version at install time, and the
// user picks the provider explicitly at download time. This entry point
// stays as the one place the server calls post-load to populate the
// cache.
func (m *ProviderAppManager) DiscoverAndRegister(ctx context.Context) error {
	if m.shutdown.Load() {
		return ErrShutdown
	}
	m.detectVersions(ctx)
	return nil
}

// detectVersions populates the version cache for enabled providers.
// Runs in parallel because the Ollama probe is the only rule that
// blocks on network I/O; everything else is a disk read.
//
// The per-provider timeout derives from m.shutdownCtx, not from the
// caller's ctx — lifetime belongs to the manager. If Stop races boot
// seeding, the probe goroutines unblock on shutdown cancellation and
// the late MergeNonEmpty is skipped via the shutdown gate below.
func (m *ProviderAppManager) detectVersions(_ context.Context) {
	cfg := m.AppsConfig()
	if cfg == nil {
		return
	}

	type versionResult struct {
		name    string
		version string
	}

	var wg sync.WaitGroup
	names := cfg.AppNames()
	results := make(chan versionResult, len(names))

	cfg.RangeApps(func(name string, svc config.ServiceConfig) bool {
		if !svc.IsEnabled() {
			return true
		}
		wg.Add(1)
		go func(name string, svc config.ServiceConfig) {
			defer wg.Done()
			defer utils.RecoverAndLog("prov_apps.detectProviderVersion")
			detectCtx, cancel := context.WithTimeout(m.shutdownCtx, constants.ClusterActionTimeout)
			defer cancel()
			version := m.detectProviderVersion(detectCtx, name, &svc)
			results <- versionResult{name, version}
		}(name, svc)
		return true
	})

	go func() {
		wg.Wait()
		close(results)
	}()

	// Skip "unknown" — only cache real versions to avoid polluting the cache.
	collected := make(map[string]string)
	for r := range results {
		if r.version != "unknown" {
			collected[r.name] = r.version
		}
	}

	// Drop the write if Stop fired while we were probing. The cache
	// owns its own lock; this gate keeps us from resurrecting state
	// after shutdown has declared the manager done.
	if m.shutdown.Load() {
		return
	}
	m.versions.MergeNonEmpty(collected)
}
