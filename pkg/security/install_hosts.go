package security

import "fmt"

// installHostAllowlist is the per-provider allowlist for binary
// download origins. Locked in Go code (not config) so a compromised
// providers/<kind>/<name>/config.yaml — whether edited on a coordinator
// or pushed via peer-sync — cannot redirect installs to attacker
// hosts. Per-provider scoping means an "ollama" config that points at
// pypi.org or an unrelated github org is rejected at validate time.
//
// Hosts here gate the URL we BUILD from install_variants[*].URLPattern
// (and VariantArtifact.URLPattern). Redirects to githubusercontent
// CDNs are followed at fetch time and revalidated against the global
// download.AllowedHosts list — that's the second layer.
//
// Adding a provider: register the minimal upstream domain set needed
// for releases. Don't put CDN/redirect hosts here — those go in the
// global download allowlist instead.
var installHostAllowlist = map[string][]string{
	// Both Linux and Windows ollama installs pull versioned archives
	// from github releases (so we can verify against the published
	// sha256sum.txt sibling). ollama.com/download has no checksum
	// surface, so we don't reach for it.
	"ollama":   {"github.com"},
	"llamacpp": {"github.com"},
}

// ValidateInstallHost returns nil when rawURL's scheme is https and its
// host matches the per-provider allowlist. Providers without a
// registered allowlist (pip-only installers, registries) bypass —
// callers that require enforcement must also assert provider has an
// entry via HasInstallHostAllowlist.
func ValidateInstallHost(provider, rawURL string) error {
	allowed, ok := installHostAllowlist[provider]
	if !ok {
		return nil
	}
	if err := ValidateDownloadURL(rawURL, allowed); err != nil {
		return fmt.Errorf("provider %q install URL rejected: %w", provider, err)
	}
	return nil
}

// HasInstallHostAllowlist reports whether the provider has a registered
// per-provider host pin. Lets callers distinguish "explicitly skipped"
// from "we'd validate but the provider isn't pinned yet".
func HasInstallHostAllowlist(provider string) bool {
	_, ok := installHostAllowlist[provider]
	return ok
}
