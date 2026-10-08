package install

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
)

// ResolvedPackage is resolver evidence, not permission to add a new root package.
type ResolvedPackage = fsroot.ManifestPackage

// ParsePipReport refuses direct references and unhashed artifacts before installation.
func ParsePipReport(data []byte, roots map[string]string) ([]ResolvedPackage, error) {
	if len(data) > 8<<20 {
		return nil, fmt.Errorf("dependency report exceeds bound")
	}
	var report struct {
		Version string `json:"version"`
		Install []struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
			Requested bool `json:"requested"`
			Direct    bool `json:"is_direct"`
			Download  struct {
				URL     string `json:"url"`
				Archive struct {
					Hashes map[string]string `json:"hashes"`
				} `json:"archive_info"`
			} `json:"download_info"`
		} `json:"install"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("dependency report: %w", err)
	}
	if report.Version != "1" || len(report.Install) == 0 || len(report.Install) > 512 {
		return nil, fmt.Errorf("unsupported or empty dependency report")
	}
	inventory := make([]ResolvedPackage, 0, len(report.Install))
	seen := map[string]bool{}
	for _, entry := range report.Install {
		name := strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(entry.Metadata.Name))
		if !config.ValidDistribution(name) || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate resolved package")
		}
		seen[name] = true
		if entry.Direct {
			return nil, fmt.Errorf("direct-reference dependency %q refused", name)
		}
		if err := config.ValidateSpecifier("==" + entry.Metadata.Version); err != nil {
			return nil, err
		}
		if err := config.ValidateInstallArtifactURL(entry.Download.URL); err != nil {
			return nil, fmt.Errorf("artifact provenance for %s: %w", name, err)
		}
		hash := entry.Download.Archive.Hashes["sha256"]
		if len(hash) != 64 || strings.IndexFunc(hash, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) >= 0 {
			return nil, fmt.Errorf("missing SHA256 for %s", name)
		}
		if entry.Requested {
			if _, ok := roots[name]; !ok {
				return nil, fmt.Errorf("unexpected root package %s", name)
			}
		}
		if constraint, root := roots[name]; root {
			if !entry.Requested {
				return nil, fmt.Errorf("resolver did not mark root package %s requested", name)
			}
			if err := upstream.VersionAllowed(entry.Metadata.Version, constraint); err != nil {
				return nil, fmt.Errorf("resolver root %s: %w", name, err)
			}
		}
		inventory = append(inventory, ResolvedPackage{Name: name, Version: entry.Metadata.Version, Requested: entry.Requested, ArtifactURL: entry.Download.URL, SHA256: hash})
	}
	for name := range roots {
		if !seen[name] {
			return nil, fmt.Errorf("resolver omitted root package %s", name)
		}
	}
	slices.SortFunc(inventory, func(a, b ResolvedPackage) int { return strings.Compare(a.Name, b.Name) })
	return inventory, nil
}

// InventoryRequirements pins the accepted resolution without exposing executable input.
func InventoryRequirements(inventory []ResolvedPackage) string {
	var out strings.Builder
	for _, pkg := range inventory {
		fmt.Fprintf(&out, "%s==%s --hash=sha256:%s\n", pkg.Name, pkg.Version, pkg.SHA256)
	}
	return out.String()
}
