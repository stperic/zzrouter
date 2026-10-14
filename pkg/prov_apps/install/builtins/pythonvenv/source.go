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
	if manifest == nil || manifest.Recipe == nil || manifest.Recipe.Source == nil || manifest.PlanID == "" || manifest.Interpreter == nil || manifest.Interpreter.Path == "" || manifest.Interpreter.Version == "" || len(manifest.Inventory) == 0 || manifest.Version != manifest.Recipe.Source.Version || manifest.RecipeFingerprint != install.Fingerprint(*manifest.Recipe) {
		return "", true, fmt.Errorf("source installation has no complete managed record; reinstall required")
	}
	return manifest.Version, true, nil
}
