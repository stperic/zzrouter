// Per-provider directory loader. Each provider lives in its own
// subdirectory under `<dir>/<kind>/<name>/config.yaml`, with an
// optional sibling `schema.yaml` (consumed by pkg/prov_apps/schema).
// Kind is the top subdirectory (`on-demand`, `external`, `cloud`,
// `registries`); name is the inner directory. The tree is the index —
// there is no merged provider config. Settings live in
// `<dir>/settings.yaml`.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/config/schema"
	"github.com/stperic/zzrouter/pkg/constants"
)

// knownKinds maps each providers/<subdir>/ to its Kind enum.
// Directory names use "registries" (plural) to read naturally in the
// filesystem tree; the Kind enum itself is singular.
var knownKinds = map[string]Kind{
	"on-demand":  KindOnDemand,
	"external":   KindExternal,
	"cloud":      KindCloud,
	"registries": KindRegistry,
}

// kindDirOrder controls the iteration order for deterministic loading
// + duplicate-name diagnostics. Matches the order in marshal/save paths.
var kindDirOrder = []string{"on-demand", "external", "cloud", "registries"}

// loadAppsConfigFromDir walks the per-provider directory tree and
// returns a fully populated AppsConfig. Per-body schema validation +
// strict-decode are run on each file; duplicate names across kinds are
// rejected. Settings are loaded from `<dir>/settings.yaml` if present.
func loadAppsConfigFromDir(dir string) (*AppsConfig, error) {
	cfg := &AppsConfig{
		Version:   "1.0",
		providers: make(map[string]Provider),
	}

	for _, kindKey := range kindDirOrder {
		subdir := filepath.Join(dir, kindKey)
		entries, err := os.ReadDir(subdir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", subdir, err)
		}
		kind := knownKinds[kindKey]

		// Provider names are the subdirectory names under this kind.
		// Resolve symlinks so a symlink-to-dir (e.g. shared provider tree)
		// is treated as the directory it targets, not warned as a stray file.
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if isProviderDir(subdir, e) {
				names = append(names, e.Name())
				continue
			}
			warnLegacyProviderFile(subdir, e)
		}
		sort.Strings(names)

		for _, name := range names {
			fpath := filepath.Join(subdir, name, "config.yaml")
			if _, err := os.Stat(fpath); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("stat %s: %w", fpath, err)
			}

			if _, dup := cfg.providers[name]; dup {
				return nil, fmt.Errorf("%w: provider %q is declared in multiple kinds", ErrInvalidConfig, name)
			}

			body, err := os.ReadFile(fpath)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", fpath, err)
			}
			body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
			body = FixWindowsPathsInYAML(body)

			p, err := decodeProviderBody(kind, name, body)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", kindKey, name, err)
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("%s/%s: %w", kindKey, name, err)
			}

			// Resolve relative container volume paths for on-demand providers.
			if od, ok := p.(*OnDemandProvider); ok && od.Runtime.Container != nil {
				resolveContainerVolumes(od.Runtime.Container, dir)
			}
			// Env var expansion in Defaults.Environment.
			expandDefaultsEnv(providerDefaults(p))

			cfg.providers[name] = p
		}
	}

	// Settings file (optional but expected — install templates ship it).
	settingsPath := filepath.Join(dir, "settings.yaml")
	if data, err := os.ReadFile(settingsPath); err == nil {
		data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		var s UnifiedSettings
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("settings.yaml: %w", err)
		}
		cfg.Settings = s
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read settings.yaml: %w", err)
	}

	if len(cfg.providers) == 0 {
		return nil, fmt.Errorf("at least one provider must be defined")
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.providers)) {
		p := cfg.providers[name]
		if err := cfg.checkVariantNames(name, providerToServiceConfig(p)); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", kindDirKeyFor(p.Kind()), name, err)
		}
	}

	return cfg, nil
}

// decodeProviderBody validates body against the kind's JSON schema, then
// strict-decodes into the corresponding typed Provider. Name is injected
// from the filesystem stem — never from the YAML body (the typed structs
// intentionally have `yaml:"-"` on Name).
// DecodeProviderBytes runs the same JSON-Schema + strict-YAML pipeline a
// directory load uses, returning the parsed Provider. Caller is
// responsible for invoking Provider.Validate() if cross-field invariants
// (e.g. wire_endpoints mutual exclusion) need to be enforced. Used by the
// peer-sync receiver to fail closed at the wire on malformed inbound
// configs before they touch disk.
func DecodeProviderBytes(kind Kind, name string, body []byte) (Provider, error) {
	return decodeProviderBody(kind, name, body)
}

func decodeProviderBody(kind Kind, name string, body []byte) (Provider, error) {
	switch kind {
	case KindOnDemand:
		if err := schema.ValidateOnDemand(body); err != nil {
			return nil, err
		}
		var p OnDemandProvider
		if err := strictDecode(body, &p); err != nil {
			return nil, err
		}
		p.Name = name
		return &p, nil
	case KindExternal:
		if err := schema.ValidateExternal(body); err != nil {
			return nil, err
		}
		var p ExternalProvider
		if err := strictDecode(body, &p); err != nil {
			return nil, err
		}
		p.Name = name
		return &p, nil
	case KindCloud:
		if err := schema.ValidateCloud(body); err != nil {
			return nil, err
		}
		var p CloudProvider
		if err := strictDecode(body, &p); err != nil {
			return nil, err
		}
		p.Name = name
		return &p, nil
	case KindRegistry:
		if err := schema.ValidateRegistry(body); err != nil {
			return nil, err
		}
		var p SearchRegistry
		if err := strictDecode(body, &p); err != nil {
			return nil, err
		}
		p.Name = name
		return &p, nil
	default:
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
}

// legacyWarnOnce gates the stray-yaml warnings to fire once per process
// rather than on every config reload — the condition is steady-state
// and operators can't fix it without a redeploy of the templates.
var legacyWarnOnce sync.Map // path -> struct{}

// isProviderDir reports whether e is (or symlinks to) a directory under
// subdir. Symlinks are followed once so shared provider trees mounted
// via symlink load correctly instead of being warned as stray files.
func isProviderDir(subdir string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	st, err := os.Stat(filepath.Join(subdir, e.Name()))
	return err == nil && st.IsDir()
}

// warnLegacyProviderFile emits a one-shot WARN for a stray <kind>/<name>.yaml
// or .yml entry that the directory loader ignores. Skips dotfiles, editor
// junk (.swp, .bak, ~), and non-regular files.
func warnLegacyProviderFile(subdir string, e os.DirEntry) {
	name := e.Name()
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "#") {
		return
	}
	if !e.Type().IsRegular() {
		return
	}
	if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
		return
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
	path := filepath.Join(subdir, name)
	if _, loaded := legacyWarnOnce.LoadOrStore(path, struct{}{}); loaded {
		return
	}
	slog.Warn("ignoring stray provider config; only the directory form is loaded",
		"path", path,
		"expected", filepath.Join(subdir, stem, "config.yaml"))
}

func strictDecode(body []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// providerDefaults returns the AppDefaultsConfig pointer for any provider
// kind that carries one (on-demand + external today). Cloud and registry
// return nil — they don't ship model-launch defaults.
func providerDefaults(p Provider) *AppDefaultsConfig {
	switch tp := p.(type) {
	case *OnDemandProvider:
		return tp.Defaults
	case *ExternalProvider:
		return tp.Defaults
	default:
		return nil
	}
}

func expandDefaultsEnv(d *AppDefaultsConfig) {
	if d == nil || len(d.Environment) == 0 {
		return
	}
	expanded := make(map[string]string, len(d.Environment))
	for k, v := range d.Environment {
		expanded[k] = os.ExpandEnv(v)
	}
	d.Environment = expanded
}

func resolveContainerVolumes(c *ContainerConfig, configDir string) {
	if c == nil || len(c.Volumes) == 0 {
		return
	}
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return
	}
	resolved := make([]string, len(c.Volumes))
	for i, v := range c.Volumes {
		resolved[i] = resolveVolumePath(v, abs)
	}
	c.Volumes = resolved
}

// providerToServiceConfig is the inverse of serviceConfigToProvider.
// Used by LookupApp/RangeApps/UpdateApp to synthesize a legacy
// ServiceConfig view of a typed Provider for callers that still take
// ServiceConfig parameters. Fields the kind-split discards (like
// AppType) get sane defaults — the collapse is one-way.
func providerToServiceConfig(p Provider) ServiceConfig {
	switch tp := p.(type) {
	case *OnDemandProvider:
		return ServiceConfig{
			Install:         tp.Install,
			Features:        tp.Features,
			Enabled:         tp.Enabled,
			Name:            tp.Name,
			Description:     tp.Description,
			Protocol:        tp.Protocol,
			Mode:            constants.AppModeOnDemand,
			PinnedVersion:   tp.PinnedVersion,
			VersionSource:   tp.VersionSource,
			Platforms:       tp.Platforms,
			Requirements:    tp.Requirements,
			InstallVariants: tp.InstallVariants,
			Capabilities:    tp.Capabilities,
			Discovery:       tp.Discovery,
			Defaults:        tp.Defaults,
			ModelDefaults:   tp.ModelDefaults,
			Models:          tp.Models,
			Nodes:           tp.Nodes,
			Search:          tp.Search,
			Runtime:         onDemandRuntimeToLegacy(tp.Runtime),
		}
	case *ExternalProvider:
		return ServiceConfig{
			Features:        tp.Features,
			Enabled:         tp.Enabled,
			Name:            tp.Name,
			Description:     tp.Description,
			Protocol:        tp.Protocol,
			Mode:            constants.AppModeExternal,
			PinnedVersion:   tp.PinnedVersion,
			VersionSource:   tp.VersionSource,
			Requirements:    tp.Requirements,
			InstallVariants: tp.InstallVariants,
			Capabilities:    tp.Capabilities,
			Discovery:       tp.Discovery,
			Defaults:        tp.Defaults,
			Models:          tp.Models,
			Nodes:           tp.Nodes,
			Service:         tp.Service,
			Search:          tp.Search,
			Runtime:         externalRuntimeToLegacy(tp.Runtime),
		}
	case *CloudProvider:
		// Cloud providers are OpenAI-shaped by contract (the kind itself
		// is the constraint — there's no cloud provider that doesn't
		// speak OpenAI wire format). Synthesize the Protocol so
		// downstream callers like deriveCapabilities can identify the
		// protocol family without special-casing cloud.
		return ServiceConfig{
			Features:     tp.Features,
			Enabled:      tp.Enabled,
			Name:         tp.Name,
			Description:  tp.Description,
			Protocol:     ProtocolOpenAI,
			Mode:         constants.AppModeCloud,
			Capabilities: tp.Capabilities,
			Search:       tp.Search,
			Runtime:      cloudRuntimeToLegacy(tp.Runtime),
		}
	case *SearchRegistry:
		return ServiceConfig{
			Enabled:     tp.Enabled,
			Name:        tp.Name,
			Description: tp.Description,
			Mode:        constants.AppModeRegistry,
			Search:      tp.Search,
			Runtime:     registryRuntimeToLegacy(tp.Runtime),
		}
	}
	return ServiceConfig{}
}

func onDemandRuntimeToLegacy(rt OnDemandRuntime) *AppRuntimeConfig {
	return &AppRuntimeConfig{
		PortRange:             rt.PortRange,
		BasePort:              rt.BasePort,
		KeepAlive:             rt.KeepAlive,
		MaxConcurrentRequests: rt.MaxConcurrentRequests,
		QueueTimeout:          rt.QueueTimeout,
		Execution:             rt.Execution,
		HealthCheck:           rt.HealthCheck,
		Container:             rt.Container,
		Resources:             rt.Resources,
	}
}

func externalRuntimeToLegacy(rt ExternalRuntime) *AppRuntimeConfig {
	return &AppRuntimeConfig{
		Endpoint:    rt.Endpoint,
		ModelURLFmt: rt.ModelURL,
		KeepAlive:   rt.KeepAlive,
		API:         rt.API,
		HealthCheck: rt.HealthCheck,
		// External daemons in the legacy shape always carried execution.type=api
		// as a cosmetic "it talks to an HTTP API" tag. Re-emit it so any legacy
		// reader that asserts non-nil Execution keeps working.
		Execution: ExecutionConfig{Type: "api"},
	}
}

func cloudRuntimeToLegacy(rt CloudRuntime) *AppRuntimeConfig {
	apiCopy := rt.API
	return &AppRuntimeConfig{
		Endpoint:          rt.Endpoint,
		ModelURLFmt:       rt.ModelURL,
		ModelsPath:        rt.ModelsPath,
		ModelsResponseKey: rt.ModelsResponseKey,
		API:               &apiCopy,
		HealthCheck:       rt.HealthCheck,
		Execution:         ExecutionConfig{Type: "api"},
	}
}

func registryRuntimeToLegacy(rt RegistryRuntime) *AppRuntimeConfig {
	return &AppRuntimeConfig{
		Endpoint:    rt.Endpoint,
		ModelURLFmt: rt.ModelURL,
		Execution:   ExecutionConfig{Type: "api"},
	}
}
