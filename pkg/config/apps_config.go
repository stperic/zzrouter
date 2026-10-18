// Package config provides OS-agnostic configuration file management for zzRouter
// AppsConfig loading and management
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/utils"
)

// ErrProviderExists is returned by AddApp when a provider with the given
// name is already registered. Callers that need to distinguish collision
// from other failure modes should use errors.Is rather than substring
// matching on the returned message.
var ErrProviderExists = errors.New("provider already exists")

// ErrProviderNotFound is returned by mutation methods when the named
// provider is absent from the config. HTTP callers should map this to
// 404 instead of 500. Use errors.Is rather than substring matching.
var ErrProviderNotFound = errors.New("provider not found")

// ErrProvidersDirNotFound means no providers/ directory exists. A tree
// that exists but fails to load is a different error and must not be
// read as "no providers".
var ErrProvidersDirNotFound = errors.New("providers directory not found")

// AppsConfig represents the complete apps configuration. The typed
// providers map is the sole storage; legacy ServiceConfig is synthesized
// at the boundary by LookupApp / RangeApps for callers that haven't
// migrated to typed Provider access.
type AppsConfig struct {
	Version  string          `yaml:"version"`
	Name     string          `yaml:"name"`
	Settings UnifiedSettings `yaml:"settings"`
	// providers is unexported; mutations go through UpdateApp / AddApp,
	// which validate the post-mutation typed Provider before storing it.
	providers map[string]Provider
	// node is the node this tree is read for: LookupApp and RangeApps
	// resolve its nodes entry over provider defaults. Empty reads defaults.
	node string
}

// Fingerprint returns a stable SHA-256 over the material fields of the
// AppsConfig: version, name, settings, and every provider serialized
// through its ServiceConfig view in sorted name order. Reloaders use it
// to short-circuit with DispositionIgnored when the on-disk YAML churned
// (stat/write-back) without a material field change. nil config yields
// the zero hash — distinct from any non-nil config's hash.
//
// Walking RangeApps-equivalent in sorted order avoids map-iteration
// non-determinism while keeping the unexported providers map out of the
// public surface.
func (c *AppsConfig) Fingerprint() [32]byte {
	if c == nil {
		return [32]byte{}
	}
	h := sha256.New()
	// Scalar header fields.
	fmt.Fprintf(h, "version=%s\nname=%s\n", c.Version, c.Name)
	if buf, err := yaml.Marshal(c.Settings); err == nil {
		h.Write(buf)
	}
	// Providers in sorted name order so a benign map-iteration reorder
	// produces the same digest.
	for _, name := range c.AppNames() {
		sc, ok := c.LookupApp(name)
		if !ok {
			continue
		}
		buf, err := yaml.Marshal(sc)
		if err != nil {
			fmt.Fprintf(h, "err:%s:%s\n", name, err)
			continue
		}
		fmt.Fprintf(h, "provider=%s\n", name)
		h.Write(buf)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// UpdateApp is the single in-place mutation entry point for provider
// configs. The mutator edits a synthesized ServiceConfig view of the
// stored typed Provider; UpdateApp converts the result back to a typed
// Provider, validates it, and stores it. The stored provider is
// unchanged on any of: missing-name, mutator-returned error, conversion
// failure, or post-mutation Validate failure — any partial mutations
// the closure made to its local sc are discarded with the closure.
func (c *AppsConfig) UpdateApp(name string, mutate func(*ServiceConfig) error) error {
	if c == nil || c.providers == nil {
		return fmt.Errorf("providers config not initialized")
	}
	p, exists := c.providers[name]
	if !exists {
		return fmt.Errorf("provider '%s' not found", name)
	}
	// The conversion shares p's maps, so mutate a copy: a refused
	// mutation must leave the stored provider as it was.
	cp, err := cloneProvider(p)
	if err != nil {
		return fmt.Errorf("UpdateApp(%q): %w", name, err)
	}
	sc := providerToServiceConfig(cp)
	if err := mutate(&sc); err != nil {
		return err
	}
	newP, err := serviceConfigToProvider(name, sc)
	if err != nil {
		return fmt.Errorf("UpdateApp(%q): %w", name, err)
	}
	if err := newP.Validate(); err != nil {
		return fmt.Errorf("UpdateApp(%q): %w", name, err)
	}
	if err := c.checkVariantNames(name, sc); err != nil {
		return fmt.Errorf("UpdateApp(%q): %w", name, err)
	}
	c.providers[name] = newP
	return nil
}

// clone copies c with a providers map of its own. The providers are shared:
// UpdateApp replaces one rather than editing it.
func (c *AppsConfig) clone() *AppsConfig {
	cp := *c
	cp.providers = maps.Clone(c.providers)
	return &cp
}

// AddApp inserts a new provider config from a legacy ServiceConfig.
// Returns an error when a provider already exists under that name, or
// when sc is invalid (unknown mode, type conversion failure, or per-kind
// validation failure). Callers that want upsert semantics should check
// Find(name) first, or route a mutation through UpdateApp.
func (c *AppsConfig) AddApp(name string, sc ServiceConfig) error {
	if c == nil {
		return fmt.Errorf("providers config not initialized")
	}
	if c.providers == nil {
		c.providers = make(map[string]Provider)
	}
	if _, exists := c.providers[name]; exists {
		return fmt.Errorf("provider %q: %w (use UpdateApp to mutate)", name, ErrProviderExists)
	}
	p, err := serviceConfigToProvider(name, sc)
	if err != nil {
		return fmt.Errorf("AddApp(%q): %w", name, err)
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("AddApp(%q): %w", name, err)
	}
	if err := c.checkVariantNames(name, sc); err != nil {
		return fmt.Errorf("AddApp(%q): %w", name, err)
	}
	c.providers[name] = p
	return nil
}

// LookupApp returns a synthesized ServiceConfig view of a provider for
// callers that take ServiceConfig parameters. The bool is false if no
// provider exists under that name. Mutations to the returned value have
// no effect on the stored provider — use UpdateApp instead.
func (c *AppsConfig) LookupApp(name string) (ServiceConfig, bool) {
	if c == nil || c.providers == nil {
		return ServiceConfig{}, false
	}
	p, ok := c.providers[name]
	if !ok {
		return ServiceConfig{}, false
	}
	return c.view(p), true
}

// view is p as this tree's node reads it: declared values, with enable
// questions answered for that node.
func (c *AppsConfig) view(p Provider) ServiceConfig {
	sc := providerToServiceConfig(p)
	sc.node = c.node
	return sc
}

// AppNames returns a snapshot of the configured provider names.
func (c *AppsConfig) AppNames() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.providers))
	for name := range c.providers {
		out = append(out, name)
	}
	return out
}

// RangeApps invokes fn for each (name, synthesized ServiceConfig) pair.
// Order is unspecified. Halts iteration if fn returns false. Mutations
// to the synthesized ServiceConfig have no effect — use UpdateApp.
func (c *AppsConfig) RangeApps(fn func(name string, sc ServiceConfig) bool) {
	if c == nil {
		return
	}
	for name, p := range c.providers {
		if !fn(name, c.view(p)) {
			return
		}
	}
}

// Find returns the typed provider for a given name, or nil if absent.
// Callers that need a kind-specific field should prefer the typed
// accessors (GetOnDemand, GetCloud, etc.) — Find is for kind-agnostic
// reads where a type switch is unavoidable.
func (c *AppsConfig) Find(name string) Provider {
	if c == nil || c.providers == nil {
		return nil
	}
	return c.providers[name]
}

// GetOnDemand returns the on-demand provider with the given name, or nil
// if absent or registered under a different kind. Compile-time-safe
// alternative to Find + type-assertion.
func (c *AppsConfig) GetOnDemand(name string) *OnDemandProvider {
	if p := c.Find(name); p != nil {
		if od, ok := p.(*OnDemandProvider); ok {
			return od
		}
	}
	return nil
}

// GetExternal returns the external provider with the given name, or nil.
func (c *AppsConfig) GetExternal(name string) *ExternalProvider {
	if p := c.Find(name); p != nil {
		if ep, ok := p.(*ExternalProvider); ok {
			return ep
		}
	}
	return nil
}

// GetCloud returns the cloud provider with the given name, or nil.
func (c *AppsConfig) GetCloud(name string) *CloudProvider {
	if p := c.Find(name); p != nil {
		if cp, ok := p.(*CloudProvider); ok {
			return cp
		}
	}
	return nil
}

// GetRegistry returns the search registry with the given name, or nil.
func (c *AppsConfig) GetRegistry(name string) *SearchRegistry {
	if p := c.Find(name); p != nil {
		if sr, ok := p.(*SearchRegistry); ok {
			return sr
		}
	}
	return nil
}

// AllProviders returns a snapshot of the typed provider map. Use when
// the caller needs to iterate every provider and dispatch by Kind; for
// kind-scoped iteration prefer OnDemandProviders / CloudProviders / etc.
// The returned map is a copy — safe to range without holding any lock.
func (c *AppsConfig) AllProviders() map[string]Provider {
	if c == nil {
		return map[string]Provider{}
	}
	out := make(map[string]Provider, len(c.providers))
	for name, p := range c.providers {
		out[name] = p
	}
	return out
}

// OnDemandProviders returns a snapshot map of all on-demand providers.
// Commits 5a-5c use this for kind-scoped iteration (installer hot paths,
// port allocator) — the alternative is a range+type-switch at every call
// site, which the cold review flagged as a bloat risk.
func (c *AppsConfig) OnDemandProviders() map[string]*OnDemandProvider {
	out := make(map[string]*OnDemandProvider)
	if c == nil {
		return out
	}
	for name, p := range c.providers {
		if od, ok := p.(*OnDemandProvider); ok {
			out[name] = od
		}
	}
	return out
}

// ExternalProviders returns a snapshot map of all external-daemon
// providers (ollama today).
func (c *AppsConfig) ExternalProviders() map[string]*ExternalProvider {
	out := make(map[string]*ExternalProvider)
	if c == nil {
		return out
	}
	for name, p := range c.providers {
		if ep, ok := p.(*ExternalProvider); ok {
			out[name] = ep
		}
	}
	return out
}

// CloudProviders returns a snapshot map of all cloud providers.
func (c *AppsConfig) CloudProviders() map[string]*CloudProvider {
	out := make(map[string]*CloudProvider)
	if c == nil {
		return out
	}
	for name, p := range c.providers {
		if cp, ok := p.(*CloudProvider); ok {
			out[name] = cp
		}
	}
	return out
}

// SearchRegistries returns a snapshot map of all search registries.
func (c *AppsConfig) SearchRegistries() map[string]*SearchRegistry {
	out := make(map[string]*SearchRegistry)
	if c == nil {
		return out
	}
	for name, p := range c.providers {
		if sr, ok := p.(*SearchRegistry); ok {
			out[name] = sr
		}
	}
	return out
}

// IsAppEnabled checks if an app is enabled. Unknown providers default to
// enabled (legacy behavior — preserves the auto-discovery semantics that
// callers in pkg/runs depend on).
// Note: implements runs.AppConfigProvider without importing it to avoid circularity.
func (c *AppsConfig) IsAppEnabled(appType string) bool {
	if sc, exists := c.LookupApp(appType); exists {
		return sc.IsEnabled()
	}
	return true
}

// LoadAppsConfig loads app configuration from a per-provider directory
// tree. Each provider lives at `<dir>/<kind>/<name>.yaml`; the directory
// is the index. See loadAppsConfigFromDir for the parsing contract.
func LoadAppsConfig(dir string) (*AppsConfig, error) {
	cfg, err := loadAppsConfigFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("providers config '%s': %w", dir, err)
	}
	return cfg, nil
}

// GetEnabledApps returns a list of enabled app names
func (c *AppsConfig) GetEnabledApps() []string {
	var enabled []string
	c.RangeApps(func(name string, appCfg ServiceConfig) bool {
		if appCfg.IsEnabled() {
			enabled = append(enabled, name)
		}
		return true
	})
	return enabled
}

// GetOnDemandApps returns a list of on-demand app names.
func (c *AppsConfig) GetOnDemandApps() []string {
	var onDemand []string
	c.RangeApps(func(name string, appCfg ServiceConfig) bool {
		if appCfg.IsEnabled() && appCfg.LaunchesProcess() {
			onDemand = append(onDemand, name)
		}
		return true
	})
	return onDemand
}

// GetEndpointApps returns apps that have their own running endpoint
// (service, external, cloud). Used for routing — these don't need process launching.
func (c *AppsConfig) GetEndpointApps() []string {
	var apps []string
	c.RangeApps(func(name string, appCfg ServiceConfig) bool {
		if appCfg.IsEnabled() && appCfg.HasEndpoint() {
			apps = append(apps, name)
		}
		return true
	})
	return apps
}

// ============================================================================
// DRY: Centralized App Config Loading
// ============================================================================

// LoadAppsConfigFromStandardLocations loads the providers directory from
// XDG-compliant location. DRY utility to eliminate duplicate loading
// logic across the codebase. The returned path is the directory.
func LoadAppsConfigFromStandardLocations() (*AppsConfig, string, error) {
	configDir, err := findAppsConfigDir()
	if err != nil {
		return nil, "", err
	}

	// Load config from found location
	config, err := LoadAppsConfig(configDir)
	if err != nil {
		return nil, "", fmt.Errorf("failed to load apps config from %s: %w", configDir, err)
	}

	return config, configDir, nil
}

// findAppsConfigDir returns the XDG providers/ directory or
// ErrProvidersDirNotFound.
func findAppsConfigDir() (string, error) {
	cm := NewConfigManager("zzrouter")
	if dir := cm.FindAppsConfigDir(); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("%w in %s", ErrProvidersDirNotFound, cm.GetNodeConfigDir())
}

// EnsureNodeSpecFor is the exported wrapper over ensureNodeSpec for
// cross-package merge-patch mutators (internal/server).
func EnsureNodeSpecFor(sc *ServiceConfig, node string) NodeSpec {
	return ensureNodeSpec(sc, node)
}

// EnsureModelSpecFor is the exported wrapper over ensureModelSpec.
func EnsureModelSpecFor(sc *ServiceConfig, model string) ModelSpec {
	return ensureModelSpec(sc, model)
}

// CleanupNodeSpecFor is the exported wrapper over cleanupNodeSpec.
func CleanupNodeSpecFor(sc *ServiceConfig, node string) { cleanupNodeSpec(sc, node) }

// CleanupModelSpecFor is the exported wrapper over cleanupModelSpec.
func CleanupModelSpecFor(sc *ServiceConfig, model string) { cleanupModelSpec(sc, model) }

// CleanupNodeModelSpecFor is the exported wrapper over cleanupNodeModelSpec.
func CleanupNodeModelSpecFor(sc *ServiceConfig, node, model string) {
	cleanupNodeModelSpec(sc, node, model)
}

// ensureNodeSpec returns a writable NodeSpec, allocating the Nodes map if needed.
func ensureNodeSpec(sc *ServiceConfig, node string) NodeSpec {
	if sc.Nodes == nil {
		sc.Nodes = make(map[string]NodeSpec)
	}
	spec := sc.Nodes[node]
	return spec
}

// ensureModelSpec returns a writable ModelSpec, allocating the Models map if needed.
func ensureModelSpec(sc *ServiceConfig, model string) ModelSpec {
	if sc.Models == nil {
		sc.Models = make(map[string]ModelSpec)
	}
	spec := sc.Models[model]
	return spec
}

// cleanupNodeSpec drops an empty node entry; empties the Nodes map if last.
func cleanupNodeSpec(sc *ServiceConfig, node string) {
	spec, ok := sc.Nodes[node]
	if !ok {
		return
	}
	if spec.Enabled == nil && len(spec.Parameters) == 0 && len(spec.Environment) == 0 && len(spec.Endpoints) == 0 && len(spec.Models) == 0 && spec.Install == nil {
		delete(sc.Nodes, node)
	}
	if len(sc.Nodes) == 0 {
		sc.Nodes = nil
	}
}

// cleanupModelSpec drops an empty model entry; empties the Models map if last.
func cleanupModelSpec(sc *ServiceConfig, model string) {
	spec, ok := sc.Models[model]
	if !ok {
		return
	}
	if spec.IsEmpty() {
		delete(sc.Models, model)
	}
	if len(sc.Models) == 0 {
		sc.Models = nil
	}
}

// cleanupNodeModelSpec drops an empty (node, model) cell and trims the node
// entry if it then has no remaining content.
func cleanupNodeModelSpec(sc *ServiceConfig, node, model string) {
	nspec, ok := sc.Nodes[node]
	if !ok {
		return
	}
	if nspec.Models == nil {
		return
	}
	nm, ok := nspec.Models[model]
	if !ok {
		return
	}
	if len(nm.Parameters) == 0 && len(nm.Environment) == 0 && len(nm.Endpoints) == 0 {
		delete(nspec.Models, model)
	}
	if len(nspec.Models) == 0 {
		nspec.Models = nil
	}
	sc.Nodes[node] = nspec
	cleanupNodeSpec(sc, node)
}

// SaveToDir writes the AppsConfig to a per-provider directory tree
// rooted at dir. Each provider becomes `<dir>/<kind>/<name>/config.yaml`
// and settings becomes `<dir>/settings.yaml`. Writes are atomic
// (temp+rename per file). Each config.yaml is prefixed with a MANAGED
// FILE banner warning workers off manual edits (plan §5.2). Stale
// provider subdirs (present on disk but no longer in c.providers) are
// removed so the tree mirrors the in-memory config.
func (c *AppsConfig) SaveToDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create providers dir: %w", err)
	}

	// Track provider subdirs we wrote per kind so we can GC the rest.
	wroteDirs := make(map[string]map[string]bool)

	for _, kindKey := range kindDirOrder {
		kind := knownKinds[kindKey]
		subdir := filepath.Join(dir, kindKey)
		wroteDirs[kindKey] = make(map[string]bool)

		bodies := make(map[string][]byte)
		for name, p := range c.providers {
			if p.Kind() != kind {
				continue
			}
			data, err := marshalProviderBody(p)
			if err != nil {
				return fmt.Errorf("marshal %s/%s: %w", kindKey, name, err)
			}
			data = ensureParameterQuotes(data)
			data = FixWindowsPathsInYAML(data)
			bodies[name] = prependManagedBanner(data)
		}

		if len(bodies) > 0 {
			if err := os.MkdirAll(subdir, 0755); err != nil {
				return fmt.Errorf("create %s: %w", subdir, err)
			}
		}
		for name, data := range bodies {
			providerDir := filepath.Join(subdir, name)
			if err := os.MkdirAll(providerDir, 0755); err != nil {
				return fmt.Errorf("create %s: %w", providerDir, err)
			}
			fpath := filepath.Join(providerDir, "config.yaml")
			if err := writeYAMLIfChanged(fpath, data); err != nil {
				return fmt.Errorf("write %s: %w", fpath, err)
			}
			wroteDirs[kindKey][name] = true
		}

		// Remove stale provider subdirs this save did not write.
		if entries, err := os.ReadDir(subdir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				if !wroteDirs[kindKey][e.Name()] {
					_ = os.RemoveAll(filepath.Join(subdir, e.Name()))
				}
			}
		}
	}

	// settings.yaml at the providers root.
	var sbuf bytes.Buffer
	enc := yaml.NewEncoder(&sbuf)
	enc.SetIndent(2)
	if err := enc.Encode(&c.Settings); err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	_ = enc.Close()
	settingsPath := filepath.Join(dir, "settings.yaml")
	if err := writeYAMLIfChanged(settingsPath, sbuf.Bytes()); err != nil {
		return fmt.Errorf("write settings.yaml: %w", err)
	}

	return nil
}

// managedBanner is the top-of-file header the coordinator stamps onto
// every written config.yaml. Workers receive the same bytes on peer-sync
// so the banner travels with the file (plan §5.2).
const managedBanner = "# MANAGED FILE: overwritten on every cluster sync.\n# Edit on the coordinator, never here.\n"

func prependManagedBanner(data []byte) []byte {
	if bytes.HasPrefix(data, []byte("# MANAGED FILE")) {
		return data
	}
	out := make([]byte, 0, len(managedBanner)+len(data))
	out = append(out, []byte(managedBanner)...)
	out = append(out, data...)
	return out
}

// cloneProvider returns an independent copy of p: the bytes it would be
// saved as, decoded back the way a load does, so the copy holds exactly
// what a restart would.
func cloneProvider(p Provider) (Provider, error) {
	body, err := marshalProviderBody(p)
	if err != nil {
		return nil, fmt.Errorf("copy provider %q: %w", p.GetName(), err)
	}
	cp, err := decodeProviderBody(p.Kind(), p.GetName(), body)
	if err != nil {
		return nil, fmt.Errorf("copy provider %q: %w", p.GetName(), err)
	}
	return cp, nil
}

// marshalProviderBody emits a single provider as a YAML document. The
// typed Provider's yaml tags do the work; Name is intentionally omitted
// (yaml:"-") because the file stem carries it.
func marshalProviderBody(p Provider) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

// writeYAMLIfChanged writes data to path only when it differs from what
// is already there.
//
// SaveToDir re-marshals every provider on every mutation, so enabling
// one provider used to rewrite all of them: N atomic renames and N
// fresh mtimes for a change to one file. That churn is not just waste
// — an mtime is what anything watching this tree reads as "this
// changed", so a single edit announced itself as a change to
// everything.
func writeYAMLIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	return atomicWriteYAML(path, data)
}

// atomicWriteYAML writes provider-config YAML to path durably via the
// shared utils helper. Provider configs may carry cloud-API keys, so
// the destination perm is 0o600. The helper fsyncs both the file and
// the parent directory and handles the Windows rename-over-existing
// edge.
func atomicWriteYAML(path string, data []byte) error {
	return utils.AtomicWriteFile(path, data, 0o600)
}

// ensureParameterQuotes preserves YAML escapes and block scalars while quoting strings.
func ensureParameterQuotes(data []byte) []byte {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return data
	}
	var quote func(*yaml.Node)
	quote = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			for i := 1; i < len(node.Content); i += 2 {
				value := node.Content[i]
				if value.Kind == yaml.ScalarNode && value.Tag == "!!str" && value.Style == 0 {
					value.Style = yaml.DoubleQuotedStyle
				}
			}
		}
		for _, child := range node.Content {
			quote(child)
		}
	}
	quote(&document)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return data
	}
	_ = encoder.Close()
	return out.Bytes()
}

// FixWindowsPathsInYAML fixes Windows paths in YAML that cause escape character errors
// Windows paths like C:\Users\... need to be properly quoted or escaped in YAML
// This function fixes both unquoted paths and paths inside double-quoted strings
// Exported for use by other packages that write YAML files
func FixWindowsPathsInYAML(data []byte) []byte {
	// Convert to string for processing
	content := string(data)
	lines := strings.Split(content, "\n")

	// Pattern 1: Match unquoted Windows paths in YAML values
	// Matches: key: C:\path\to\file (unquoted Windows path)
	unquotedPattern := regexp.MustCompile(`^(\s+[^:]+:\s+)([A-Za-z]:\\[^\s"']+)(\s*(?:#.*)?)$`)

	// Pattern 2: Match Windows paths inside double-quoted strings that need escaping
	// Matches: key: "C:\path\to\file" (quoted but unescaped Windows path)
	// Captures: key part, the path content (without quotes), and closing quote + trailing content
	quotedPattern := regexp.MustCompile(`^(\s+[^:]+:\s+")([A-Za-z]:\\(?:[^"]|\\)+)(".*)$`)

	for i, line := range lines {
		// Skip commented lines and empty lines
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Check for unquoted Windows paths first
		if unquotedPattern.MatchString(line) {
			matches := unquotedPattern.FindStringSubmatch(line)
			if len(matches) == 4 {
				keyPart := matches[1]
				pathValue := matches[2]
				trailingPart := matches[3]

				// Escape backslashes for YAML (double them) and quote the path
				escapedPath := strings.ReplaceAll(pathValue, "\\", "\\\\")
				quotedPath := fmt.Sprintf("\"%s\"", escapedPath)

				lines[i] = keyPart + quotedPath + trailingPart
				continue
			}
		}

		// Check for Windows paths inside double-quoted strings that need escaping
		if quotedPattern.MatchString(line) {
			matches := quotedPattern.FindStringSubmatch(line)
			if len(matches) == 4 {
				keyPart := matches[1]
				pathValue := matches[2]
				trailingPart := matches[3]

				// Check if path contains unescaped backslashes (not already escaped)
				// If we find a backslash not followed by another backslash or quote, it needs escaping
				if strings.Contains(pathValue, "\\") {
					// Escape all backslashes (double them)
					escapedPath := strings.ReplaceAll(pathValue, "\\", "\\\\")
					lines[i] = keyPart + escapedPath + trailingPart
				}
			}
		}
	}

	return []byte(strings.Join(lines, "\n"))
}
