package pythonvenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/install"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/download"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

func (i *Installer) sourceRoots(ctx context.Context, snapshot install.RecipeSnapshot, directory string) (map[string]string, error) {
	source := *snapshot.Recipe.Source
	cache := filepath.Join(directory, config.SubdirCandidate, source.SHA256)
	for _, path := range []string{directory, filepath.Dir(cache), cache} {
		if err := fsroot.VerifySymlinkSafe(path); err != nil {
			return nil, err
		}
	}
	archive := filepath.Join(cache, filepath.Base(source.URL))
	if _, err := os.Stat(archive); os.IsNotExist(err) {
		if _, err := download.FetchForProvider(ctx, snapshot.Provider, source.URL, cache, nil, download.Options{MaxSize: install.SourceArchiveLimit}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return install.ReadSource(ctx, archive, "", "", source)
}

func sourceSmoke(ctx context.Context, python, directory, entrypoint string, environment []string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return runPython(ctx, python, directory, environment, "-E", "-s", "-B", filepath.Join(directory, entrypoint), "--help")
}

func (i *Installer) sourceCompletion() (string, bool, error) {
	if i.cfg.Recipe == nil {
		return "", false, nil
	}
	manifest, readErr := fsroot.ReadInstallManifest(i.cfg.Name)
	snapshot, err := i.cfg.Recipe(i.cfg.SchemaProvider, i.cfg.Name)
	if err != nil {
		if manifest != nil && manifest.Recipe != nil && manifest.Recipe.Package != nil {
			return "", false, nil
		}
		return "", true, err
	}
	source := snapshot.Recipe.Source != nil || manifest != nil && manifest.Recipe != nil && manifest.Recipe.Source != nil
	if !source {
		return "", false, nil
	}
	if readErr != nil {
		return "", true, readErr
	}
	if manifest == nil || manifest.Recipe == nil || manifest.Recipe.Source == nil || manifest.WorkingDir != snapshot.WorkingDir || manifest.PlanID == "" || manifest.Interpreter == nil || manifest.Interpreter.Path == "" || manifest.Interpreter.Version == "" || len(manifest.Inventory) == 0 || manifest.Version != manifest.Recipe.Source.Version || manifest.RecipeFingerprint != install.Fingerprint(*manifest.Recipe) {
		return "", true, fmt.Errorf("source installation has no complete managed record; reinstall required")
	}
	return manifest.Version, true, nil
}

func (i *Installer) checkSourceInstall(snapshot install.RecipeSnapshot) error {
	if err := ensureSourceData(snapshot.DataRuntime, fsroot.ProviderDir(i.cfg.Name), false); err != nil {
		return err
	}
	for _, name := range snapshot.PayloadDirectories() {
		if err := fsroot.VerifyProviderPath(fsroot.ProviderDir(i.cfg.Name), name); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(fsroot.ProviderDir(i.cfg.Name), name)); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if _, source, err := i.sourceCompletion(); !source || err != nil {
			return fmt.Errorf("Operator installation found in %s; stop it and move source and venv aside before a managed install. Data and python are kept.", fsroot.ProviderDir(i.cfg.Name)) //nolint:staticcheck // Preserve the agreed operator-facing refusal text.
		}
	}
	return nil
}

func ensureSourceData(runtime *config.AppRuntimeConfig, directory string, create bool) error {
	if runtime == nil || runtime.DataDir == "" {
		return nil
	}
	data, err := runtime.DataPath(directory)
	if err != nil {
		return err
	}
	children := []string{"."}
	if runtime.ModelStore != nil {
		for _, child := range append([]string{"."}, runtime.ModelStore.Categories...) {
			children = append(children, filepath.Join(runtime.ModelStore.Directory, child))
		}
	}
	for _, child := range children {
		if err := fsroot.VerifyProviderPath(directory, filepath.Join(runtime.DataDir, child)); err != nil {
			return err
		}
		if create {
			if err := os.MkdirAll(filepath.Join(data, child), 0700); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *Installer) sourceUninstallPlan(snapshot install.RecipeSnapshot) *install.Plan {
	directory := fsroot.ProviderDir(i.cfg.Name)
	return &install.Plan{Provider: i.cfg.Name, Platform: fsroot.CurrentPlatform(), Action: "uninstall", InstallDir: directory, Steps: []install.Step{{Number: 1, Description: "Remove managed payload and records; keep data and python", PostExec: func(ctx context.Context) error {
		if err := i.checkSourceInstall(snapshot); err != nil {
			return err
		}
		for _, name := range append(snapshot.PayloadDirectories(), config.ProviderVersionName, config.ProviderManifestName, config.ProviderManifestTemp, config.ProviderManagedName, config.SubdirCandidate, config.SubdirPrevious) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := removeManagedTree(filepath.Join(directory, name)); err != nil {
				return err
			}
		}
		return nil
	}}}}
}
