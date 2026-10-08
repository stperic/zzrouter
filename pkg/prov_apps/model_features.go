package prov_apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
)

// EnsureModelFeatures provisions selected runtime features before deployment
// completes. File features are provisioned by the model registry.
func (m *ProviderAppManager) EnsureModelFeatures(ctx context.Context, provider, modelDir string, names, optional []string, progress func(string)) error {
	svc, ok := m.ProviderConfig(provider)
	if !ok {
		return fmt.Errorf("provider %q is not configured", provider)
	}
	for _, name := range names {
		f, declared := svc.Features[name]
		if !declared {
			return fmt.Errorf("provider %q does not declare feature %q", provider, name)
		}
		if f.Runtime == "" {
			continue
		}
		qualifies, err := featurePredicate(modelDir, f.When)
		if err != nil {
			return fmt.Errorf("feature %q: %w", name, err)
		}
		if !qualifies {
			if slices.Contains(optional, name) {
				continue
			}
			return fmt.Errorf("model does not qualify for feature %q", name)
		}
		if err := m.ensureFeatureRuntime(ctx, f.Runtime, svc.Requirements, progress); err != nil {
			return fmt.Errorf("feature %q: %w", name, err)
		}
	}
	return nil
}

func (m *ProviderAppManager) ensureFeatureRuntime(ctx context.Context, name string, requirements *config.AppRequirements, progress func(string)) error {
	m.mu.Lock()
	if m.stopping.Load() || m.shutdown.Load() {
		m.mu.Unlock()
		return ErrShutdown
	}
	m.installs.wg.Add(1)
	m.mu.Unlock()
	defer m.installs.wg.Done()
	installer, err := m.installs.Installer(name)
	if err != nil {
		return err
	}
	if platforms := installer.SupportedPlatforms(); len(platforms) > 0 {
		supported := false
		for _, p := range platforms {
			if p.OS == runtime.GOOS && p.Arch == runtime.GOARCH {
				supported = true
			}
		}
		if !supported {
			return fmt.Errorf("runtime %q is unsupported on %s/%s", name, runtime.GOOS, runtime.GOARCH)
		}
	}
	if installer.IsInstalled() {
		return nil
	}
	report := installer.Preflight(ctx, requirements)
	if !report.AllOK {
		var failed []string
		for _, r := range report.Results {
			if !r.Passed {
				failed = append(failed, r.Message)
			}
		}
		return fmt.Errorf("runtime %q preflight: %s", name, strings.Join(failed, "; "))
	}
	err = m.installs.runInstall(ctx, nil, InstallRequest{Provider: name}, installer, progress)
	if errors.Is(err, ErrProviderAlreadyInstalled) {
		return nil
	}
	return err
}

func featurePredicate(modelDir, when string) (bool, error) {
	if when == "" {
		return true, nil
	}
	if when != "vision_config" {
		return false, fmt.Errorf("unknown feature predicate %q", when)
	}
	f, err := os.Open(filepath.Join(modelDir, "config.json"))
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var cfg map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return false, err
	}
	v, present := cfg[when]
	return present && string(v) != "null", nil
}
