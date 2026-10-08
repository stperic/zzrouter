// Package confighelpers provides config mutation helpers for CLI-only paths
// that run without a server process. When a server is running, use the
// centralized config stores (AppsConfigStore, NodeConfigStore) instead —
// they prevent stale-write races between concurrent handlers.
package confighelpers

import (
	"context"
	"fmt"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// ToggleProviderEnabled enables or disables a provider in the configuration.
// DRY helper used by both CLI and API endpoints. Mutates the per-provider
// directory tree (one yaml file per provider under $CONFIG/providers/).
func ToggleProviderEnabled(ctx context.Context, providerName string, enable bool) error {
	if providerName == "" {
		return fmt.Errorf("provider name cannot be empty")
	}
	if strings.ContainsAny(providerName, "/\\") {
		return fmt.Errorf("provider name contains invalid characters")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	cm := pkgConfig.NewConfigManager("zzrouter")
	appsDir := cm.GetAppsConfigDir()

	appsConfig, err := pkgConfig.LoadAppsConfig(appsDir)
	if err != nil {
		return fmt.Errorf("failed to load apps config: %w", err)
	}

	// Idempotency: no-op if already in desired state.
	current, exists := appsConfig.LookupApp(providerName)
	if !exists {
		return fmt.Errorf("provider %q not found", providerName)
	}
	if current.IsEnabled() == enable {
		return nil
	}

	// Mutate through UpdateApp so the post-mutation typed Provider is
	// re-validated before being stored.
	if err := appsConfig.UpdateApp(providerName, func(sc *pkgConfig.ServiceConfig) error {
		// architecture-exempt: package doc pins this as CLI-only; runs
		// without a server process, so there is no listener chain to
		// route through via FinalizeOnboarding/FinalizeOffboarding.
		sc.SetEnabled(enable)
		return nil
	}); err != nil {
		return err
	}

	return appsConfig.SaveToDir(appsDir)
}
