package servercli

import (
	"context"
	"fmt"
	"os"
	"sort"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
)

// fileExists is a small helper shared with init.go.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ShowProvidersTable prints a one-row-per-provider status table. Columns:
// ID, KEY, PROVIDER, MODE, ENABLED, VERSION. Version is the install-time
// version for managed providers and "cloud" / "-" for others; the CLI
// does not run any runtime detection. The live Ollama probe happens on
// the server side at startup, so by the time this table renders the
// "enabled" flag already reflects a running daemon if one was found.
func ShowProvidersTable(config *pkgConfig.AppsConfig) {
	if config == nil {
		return
	}
	rows := buildProviderRows(config)
	fmt.Printf("%-3s  %-15s  %-20s  %-12s  %-8s  %-15s\n",
		"ID", "KEY", "PROVIDER", "MODE", "ENABLED", "VERSION")
	for i, r := range rows {
		enabled := "No"
		if r.Enabled {
			enabled = "Yes"
		}
		version := r.Version
		if version == "" {
			version = "-"
		}
		fmt.Printf("%-3d  %-15s  %-20s  %-12s  %-8s  %-15s\n",
			i+1, r.Key, r.Name, r.Mode, enabled, version)
	}
	fmt.Println()
}

// ProviderRow is a flat view of a provider for CLI display.
type ProviderRow struct {
	Key     string
	Name    string
	Mode    string
	Enabled bool
	Version string
}

func buildProviderRows(config *pkgConfig.AppsConfig) []ProviderRow {
	names := config.AppNames()
	sort.Strings(names)
	rows := make([]ProviderRow, 0, len(names))
	for _, name := range names {
		svc, ok := config.LookupApp(name)
		if !ok {
			continue
		}
		rows = append(rows, ProviderRow{
			Key:     name,
			Name:    svc.Name,
			Mode:    svc.Mode,
			Enabled: svc.IsEnabled(),
			Version: installedVersionFor(name, &svc),
		})
	}
	return rows
}

func installedVersionFor(name string, svc *pkgConfig.ServiceConfig) string {
	if svc.IsCloudProvider() {
		if svc.IsCloudAvailable() {
			return "cloud"
		}
		return ""
	}
	return install.ReadInstalledVersion(name)
}

// RunProviderConfigurator prints the provider table for init/config flows.
// Auto-enablement of managed providers now happens at install time, and
// Ollama auto-enable happens at server startup — there is nothing for
// this function to mutate. It returns (nil, nil) to signal "no config
// change" so the caller's save path is skipped.
func RunProviderConfigurator(_ context.Context, config *pkgConfig.AppsConfig, _ bool) (*pkgConfig.AppsConfig, error) {
	fmt.Println()
	fmt.Println("Configured inference providers:")
	fmt.Println()
	ShowProvidersTable(config)
	fmt.Println("Install a provider:  zzrouter providers install <name>")
	fmt.Println("Enable a provider:   zzrouter providers enable <name>")
	fmt.Println()
	return nil, nil
}
