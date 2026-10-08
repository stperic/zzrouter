package config

import (
	"fmt"

	"github.com/stperic/zzrouter/pkg/security"
)

// ValidateInstallVariants checks every URLPattern declared by a
// provider's install_variants block against the per-provider host
// allowlist baked into pkg/security. The check fires on every config
// load — so a poisoned config.yaml on disk, a malicious peer-sync
// payload, or a future API surface that exposes install_variants is
// rejected at the same boundary.
//
// Providers with no per-provider allowlist registered in pkg/security
// (e.g. pip-only installers) skip — pip's own host check applies and
// install_variants don't drive their downloads.
func ValidateInstallVariants(providerName string, variants []AppInstallVariant) error {
	if !security.HasInstallHostAllowlist(providerName) {
		return nil
	}
	for i, v := range variants {
		if v.URLPattern != "" {
			if err := security.ValidateInstallHost(providerName, v.URLPattern); err != nil {
				return fmt.Errorf("install_variants[%d] (%s): %w", i, v.ID, err)
			}
		}
		for j, art := range v.Artifacts {
			if art.URLPattern != "" {
				if err := security.ValidateInstallHost(providerName, art.URLPattern); err != nil {
					return fmt.Errorf("install_variants[%d].artifacts[%d] (%s): %w", i, j, art.Filename, err)
				}
			}
		}
	}
	return nil
}
