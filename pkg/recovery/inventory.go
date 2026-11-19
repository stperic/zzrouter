// Package recovery owns offline recovery bundles. It never stops services,
// activates a restore, follows links or changes a live installation.
package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/version"
)

// InventoryFile records the paths resolved by the daemon account.
const InventoryFile = "recovery-layout.json"

// These are neither durable user state nor safe to replay after recovery. The
// access directory travels only as the database image: its live database, any
// journal or staged file, online backups and the owner-recovery proof are
// never raw sources.
func configExclusions() []string {
	return []string{"private", "models", "cache", "logs", "pids", "bin", "versions", "backups", "pairing.txt", "access", InventoryFile, "update", "update-operation.json", "update-pending-confirm.json"}
}

// Source paths are captured by the daemon, not inferred from the backup user's
// environment. Logical names address empty restore directories, never live paths.
type Source struct {
	Name     string   `json:"name"`
	Required bool     `json:"required"`
	Path     string   `json:"path"`
	Exclude  []string `json:"exclude,omitempty"`
}

// Dependency describes state that requires separate backup or reinstallation.
type Dependency struct {
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// Inventory records one installation’s recovery scope and known storage state.
type Inventory struct {
	Version             int          `json:"version"`
	StorageConfigSHA256 string       `json:"storage_config_sha256"`
	Release             string       `json:"release"`
	Platform            string       `json:"platform"`
	Node                string       `json:"node"`
	ConfigDir           string       `json:"config_dir"`
	KeyFile             string       `json:"key_file"`
	DataDir             string       `json:"data_dir"`
	ModelPaths          []string     `json:"model_paths"`
	ProviderDir         string       `json:"provider_dir"`
	Sources             []Source     `json:"sources"`
	Separate            []Dependency `json:"separate"`
}

// Capture is called after provider configuration resolves under the node account.
func Capture(ctx context.Context, cfg *config.NodeConfig, apps *config.AppsConfig) (Inventory, error) {
	dir, err := filepath.Abs(config.Paths().GetConfigDir())
	if err != nil {
		return Inventory{}, err
	}
	data, err := filepath.Abs(config.Paths().GetDataDir())
	if err != nil {
		return Inventory{}, err
	}
	modelPaths := []string{filepath.Join(data, "models")}
	for _, value := range []string{cfg.Models.Cache, cfg.Models.Shared, cfg.Models.Dir, os.Getenv("ZZROUTER_MODEL_DIR")} {
		if value == "" {
			continue
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return Inventory{}, err
		}
		modelPaths = append(modelPaths, absolute)
	}
	return describe(ctx, cfg, apps, dir, data, fsroot.ProviderRootDir(), modelPaths)
}

func describe(ctx context.Context, cfg *config.NodeConfig, apps *config.AppsConfig, dir, data, providerRoot string, modelPaths []string) (Inventory, error) {
	key, err := filepath.Abs(config.CredentialKeyPath(dir, cfg.Security.Credentials))
	if err != nil {
		return Inventory{}, err
	}
	signature, err := storageSignature(cfg, apps)
	if err != nil {
		return Inventory{}, err
	}
	result := Inventory{Version: 1, StorageConfigSHA256: signature, Release: version.Current.String(), Platform: runtime.GOOS, Node: cfg.Node.Name, ConfigDir: dir, KeyFile: key, DataDir: data, ProviderDir: providerRoot, ModelPaths: modelPaths,
		Sources:  []Source{{Name: "config", Path: dir, Exclude: configExclusions()}},
		Separate: []Dependency{{Reason: "Install the matching zzRouter release and service account before activation. Service definitions, executables and privileged install policy are not restored."}, {Path: filepath.Join(data, "models"), Reason: "Model weights and caches require a separate backup or download."}, {Reason: "Preserve external environment settings, proxy/TLS configuration, container volumes and independently managed providers separately."}}}
	if rel, err := filepath.Rel(dir, key); err == nil && !strings.HasPrefix(rel, "..") {
		result.Sources[0].Exclude = append(result.Sources[0].Exclude, filepath.ToSlash(rel))
	}
	if dir != data {
		for _, name := range []string{"pinned_runs.json", "downloads.json", "update-history.json"} {
			result.Sources = append(result.Sources, Source{Name: "data/" + name, Path: filepath.Join(data, name)})
		}
	}
	for _, path := range []string{cfg.Node.TLSCert, cfg.Node.TLSKey, cfg.Cluster.TLSCACert, cfg.Providers.Proxy.CAFile} {
		if path != "" {
			result.Separate = append(result.Separate, Dependency{Path: path, Reason: "External TLS or proxy trust material: preserve separately and verify paths before activation."})
		}
	}
	for _, modelPath := range modelPaths {
		if modelPath != "" {
			result.Separate = append(result.Separate, Dependency{Path: modelPath, Reason: "Configured model storage; preserve separately or download again."})
		}
	}
	if apps != nil {
		for _, name := range apps.AppNames() {
			app, ok := apps.LookupApp(name)
			if !ok {
				continue
			}
			provider := filepath.Join(providerRoot, name)
			if rel, err := filepath.Rel(dir, provider); err == nil && !strings.HasPrefix(rel, "..") {
				result.Sources[0].Exclude = append(result.Sources[0].Exclude, filepath.ToSlash(rel))
			}
			for _, file := range []string{config.ProviderManifestName, config.ProviderVersionName, config.ProviderManagedName} {
				result.Sources = append(result.Sources, Source{Name: "providers/" + name + "/" + file, Path: filepath.Join(provider, file)})
			}
			if app.Runtime != nil && app.Runtime.DataDir != "" {
				path, err := app.Runtime.DataPath(provider)
				if err != nil {
					return Inventory{}, err
				}
				source := Source{Name: "providers/" + name + "/" + app.Runtime.DataDir, Path: path}
				if modelPath, err := app.Runtime.ModelStorePath(provider); err == nil {
					rel, err := filepath.Rel(path, modelPath)
					if err != nil {
						return Inventory{}, err
					}
					source.Exclude = append(source.Exclude, filepath.ToSlash(rel))
					result.Separate = append(result.Separate, Dependency{Path: modelPath, Reason: "Provider-owned model weights; workflows and other provider data are included separately."})
				}
				result.Sources = append(result.Sources, source)
			}
		}
	}
	for i := range result.Sources {
		if err := ctx.Err(); err != nil {
			return Inventory{}, err
		}
		_, err := os.Lstat(result.Sources[i].Path)
		if err != nil && !os.IsNotExist(err) {
			return Inventory{}, fmt.Errorf("inspect recovery source %s: %w", result.Sources[i].Name, err)
		}
		result.Sources[i].Required = err == nil
		for _, modelPath := range modelPaths {
			rel, err := filepath.Rel(result.Sources[i].Path, modelPath)
			if err == nil && filepath.IsLocal(rel) {
				result.Sources[i].Exclude = append(result.Sources[i].Exclude, filepath.ToSlash(rel))
			}
		}
	}
	slices.SortFunc(result.Sources, func(a, b Source) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

// Save atomically records the daemon-resolved inventory during startup.
func Save(ctx context.Context, inventory Inventory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(inventory.ConfigDir, InventoryFile), data, 0o600)
}

// Read checks current declarations against the daemon’s recorded paths.
func Read(ctx context.Context, dir string) (Inventory, error) {
	var inventory Inventory
	if err := ctx.Err(); err != nil {
		return inventory, err
	}
	data, err := readBounded(filepath.Join(dir, InventoryFile), 1<<20)
	if err != nil {
		return inventory, fmt.Errorf("read recovery inventory; start this installation with the current release first: %w", err)
	}
	if err := json.Unmarshal(data, &inventory); err != nil {
		return inventory, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return inventory, err
	}
	if inventory.Version != 1 || inventory.ConfigDir != abs || len(inventory.Sources) == 0 {
		return inventory, fmt.Errorf("recovery inventory does not match this installation")
	}
	if !filepath.IsAbs(inventory.DataDir) || !filepath.IsAbs(inventory.ProviderDir) {
		return inventory, fmt.Errorf("invalid daemon recovery roots")
	}
	cfg, err := config.ReadExistingNodeConfig(dir)
	if err != nil {
		return inventory, err
	}
	apps, err := config.LoadAppsConfig(filepath.Join(dir, "providers"))
	if err != nil {
		return inventory, err
	}
	signature, err := storageSignature(cfg, apps)
	if err != nil {
		return inventory, err
	}
	if signature != inventory.StorageConfigSHA256 {
		return inventory, fmt.Errorf("storage configuration changed; start the node with this configuration to record its actual paths before backup")
	}
	current, err := describe(ctx, cfg, apps, abs, inventory.DataDir, inventory.ProviderDir, inventory.ModelPaths)
	if err != nil {
		return inventory, err
	}
	// The matching release is the daemon that last owned this state, not the backup CLI.
	current.Release, current.Platform = inventory.Release, inventory.Platform
	previous := make(map[string]bool, len(inventory.Sources))
	for _, source := range inventory.Sources {
		previous[source.Name] = source.Required
	}
	for i := range current.Sources {
		current.Sources[i].Required = current.Sources[i].Required || previous[current.Sources[i].Name]
	}
	return current, nil
}

// Bind daemon-resolved paths to the declarations that produced them. Resolving
// again under a maintenance account would substitute that account's defaults.
func storageSignature(cfg *config.NodeConfig, apps *config.AppsConfig) (string, error) {
	declarations := struct {
		ProviderRoot string
		Models       []string
		Providers    map[string][]string
	}{ProviderRoot: cfg.Providers.InstallDir, Models: []string{cfg.Models.Cache, cfg.Models.Shared, cfg.Models.Dir}, Providers: make(map[string][]string)}
	if apps != nil {
		for _, name := range apps.AppNames() {
			app, ok := apps.LookupApp(name)
			if !ok {
				continue
			}
			var paths []string
			if app.Runtime != nil {
				paths = append(paths, app.Runtime.DataDir)
				if app.Runtime.ModelStore != nil {
					paths = append(paths, app.Runtime.ModelStore.Directory)
				}
			}
			declarations.Providers[name] = paths
		}
	}
	data, err := json.Marshal(declarations)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}
