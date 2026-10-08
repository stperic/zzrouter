package harness

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the fully-resolved harness configuration. Build with one of
// the profile constructors (Base/Local/Nightly/Full) and optionally
// overlay a site YAML via Load.
//
// Public fields are stable; mutate only via the With* builders so future
// validation hooks have one place to land.
type Config struct {
	Backend     Backend         `yaml:"backend,omitempty"`
	Cluster     ClusterTopology `yaml:"cluster,omitempty"`
	Providers   []ProviderSpec  `yaml:"providers,omitempty"`
	Models      []ModelSpec     `yaml:"models,omitempty"`
	Cassettes   CassettesPolicy `yaml:"cassettes,omitempty"`
	Timeouts    Timeouts        `yaml:"timeouts,omitempty"`
	Cleanup     CleanupPolicy   `yaml:"cleanup,omitempty"`
	Rings       RingsPolicy     `yaml:"rings,omitempty"`
	ModelCache  string          `yaml:"model_cache_dir,omitempty"`
	RequiredEnv []string        `yaml:"required_env,omitempty"`

	// Site-overlay-only knobs.
	ProvidersDisabled []string `yaml:"providers_disabled,omitempty"`
}

// Default timeouts. Conservative — long-tail provider installs and 4GB
// model downloads on slow links must not flap CI.
const (
	defaultInstallProvider = 15 * time.Minute
	defaultDownloadModel   = 20 * time.Minute
	defaultLaunchRun       = 3 * time.Minute
	defaultInference       = 60 * time.Second
	defaultJobTerminal     = 30 * time.Minute
	defaultPreflight       = 30 * time.Second
)

// Load returns a fully-resolved Config: profile defaults overlaid with
// any number of YAML site files (later files win), then env validation.
//
// Profiles are addressed by name to keep the call site simple from
// `os.Getenv("ZZROUTER_E2E_PROFILE")`. Unknown profile names error.
func Load(profile string, sitePaths ...string) (*Config, error) {
	cfg, err := profileByName(profile)
	if err != nil {
		return nil, err
	}
	hasOverlay := false
	for _, p := range sitePaths {
		if p == "" {
			continue
		}
		hasOverlay = true
		if err := cfg.applyOverlay(p); err != nil {
			return nil, fmt.Errorf("overlay %s: %w", p, err)
		}
	}
	if cfg.Backend == BackendSSH && !hasOverlay {
		return nil, errors.New("SSH profile requires a site overlay; set ZZROUTER_E2E_SITES to your local site YAML")
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MustLoad is the os.Exit-on-error variant for top-level test mains.
// Returns the Config so the call site reads as a single expression.
func MustLoad(profile string, sitePaths ...string) *Config {
	cfg, err := Load(profile, sitePaths...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// profileByName dispatches to the registered profile constructors. Kept
// internal so callers can't accidentally construct a Config via
// zero-value paths that skip defaults + validation.
func profileByName(name string) (*Config, error) {
	switch name {
	case "", "base":
		return Base(), nil
	case "local":
		return Local(), nil
	case "nightly":
		return Nightly(), nil
	case "full":
		return Full(), nil
	}
	return nil, fmt.Errorf("unknown profile %q (want one of: base, local, nightly, full)", name)
}

// applyOverlay merges the YAML at path into c. yaml.v3 semantics:
// scalars and maps merge by key; slices are REPLACED when the key is
// present (a workers: list in the overlay replaces the profile's list
// wholesale, not appends). KnownFields(true) catches operator typos.
//
// Relative paths that fail against CWD are retried against the nearest
// go.mod parent so `ZZROUTER_E2E_SITES=test/e2e/configs/sites/example.yaml`
// resolves identically whether `go test` runs from the repo root or
// from inside a package dir.
func (c *Config) applyOverlay(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !filepath.IsAbs(path) {
		if alt, ok := resolveAgainstRepoRoot(path); ok {
			data, err = os.ReadFile(alt)
		}
	}
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return err
	}
	return nil
}

// resolveAgainstRepoRoot walks up from CWD looking for the sentinel
// go.mod and returns p joined against the first parent that has one.
// Returns ok=false when no go.mod is found — caller should fall back
// to whatever error the original path produced.
func resolveAgainstRepoRoot(p string) (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, p), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// applyDefaults fills zero-valued fields with package defaults. Idempotent.
func (c *Config) applyDefaults() {
	if c.Timeouts.InstallProvider == 0 {
		c.Timeouts.InstallProvider = defaultInstallProvider
	}
	if c.Timeouts.DownloadModel == 0 {
		c.Timeouts.DownloadModel = defaultDownloadModel
	}
	if c.Timeouts.LaunchRun == 0 {
		c.Timeouts.LaunchRun = defaultLaunchRun
	}
	if c.Timeouts.Inference == 0 {
		c.Timeouts.Inference = defaultInference
	}
	if c.Timeouts.JobTerminal == 0 {
		c.Timeouts.JobTerminal = defaultJobTerminal
	}
	if c.Timeouts.Preflight == 0 {
		c.Timeouts.Preflight = defaultPreflight
	}
	if c.Cassettes.Mode == "" {
		c.Cassettes.Mode = CloudReplay
	}
	if c.Backend == "" {
		c.Backend = BackendInproc
	}
}

// Validate runs structural + cross-reference checks. Catches the bugs
// that turn into 90% of "why isn't my test running" tickets.
func (c *Config) Validate() error {
	var errs []error

	if c.Cluster.Coordinator.Name == "" {
		errs = append(errs, errors.New("cluster.coordinator.name is required"))
	}
	if c.Cluster.Coordinator.Role == "" {
		c.Cluster.Coordinator.Role = RoleCoordinator
	}
	for i := range c.Cluster.Workers {
		if c.Cluster.Workers[i].Role == "" {
			c.Cluster.Workers[i].Role = RoleWorker
		}
		if c.Cluster.Workers[i].Name == "" {
			errs = append(errs, fmt.Errorf("cluster.workers[%d].name is required", i))
		}
	}

	// Provider install_on names must resolve.
	nodeNames := map[string]bool{c.Cluster.Coordinator.Name: true}
	for _, w := range c.Cluster.Workers {
		nodeNames[w.Name] = true
	}
	for _, p := range c.Providers {
		for _, n := range p.InstallOn {
			if !nodeNames[n] {
				errs = append(errs, fmt.Errorf("provider %q install_on references unknown node %q", p.Name, n))
			}
		}
	}

	// At most one Primary per (role, IsCloud) — local + cloud primaries
	// coexist so cloud-only roles don't collide with local-only roles.
	type primaryKey struct {
		role  ModelRole
		cloud bool
	}
	primary := map[primaryKey]string{}
	for _, m := range c.Models {
		if !m.Primary {
			continue
		}
		k := primaryKey{role: m.Role, cloud: m.IsCloud()}
		if prev, ok := primary[k]; ok {
			errs = append(errs, fmt.Errorf("two primary models for role %q (cloud=%v): %s and %s",
				m.Role, k.cloud, prev, m.ID))
			continue
		}
		primary[k] = m.ID
	}

	// Cassettes.Mode allowlist — applyDefaults sets replay only when
	// empty; an explicit junk value should fail validate.
	switch c.Cassettes.Mode {
	case CloudReplay, CloudRecord, CloudLive:
	default:
		errs = append(errs, fmt.Errorf("cassettes.mode %q not in {replay,record,live}", c.Cassettes.Mode))
	}

	// Required env — fail-fast at config load, not on the 47th subtest.
	for _, k := range c.RequiredEnv {
		if os.Getenv(k) == "" {
			errs = append(errs, fmt.Errorf("required env %q not set", k))
		}
	}

	// Cloud secrets only required when actually hitting cloud.
	if c.Cassettes.Mode == CloudRecord || c.Cassettes.Mode == CloudLive {
		for _, p := range c.Providers {
			if p.Cloud && p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
				errs = append(errs, fmt.Errorf("cloud provider %q requires env %q (cassettes.mode=%s)",
					p.Name, p.APIKeyEnv, c.Cassettes.Mode))
			}
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// PrimaryModel returns the model marked Primary for the given role.
// Errors when none is registered — callers should not silently guess.
func (c *Config) PrimaryModel(role ModelRole) (ModelSpec, error) {
	for _, m := range c.Models {
		if m.Role == role && m.Primary {
			return m, nil
		}
	}
	return ModelSpec{}, fmt.Errorf("no primary model for role %q", role)
}

// PrimaryProvider returns the lone provider marked Primary. Errors when
// zero or >1 are registered — both indicate a profile bug.
func (c *Config) PrimaryProvider() (ProviderSpec, error) {
	var found []ProviderSpec
	for _, p := range c.Providers {
		if p.Primary && !p.Disabled {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return ProviderSpec{}, errors.New("no primary provider")
	case 1:
		return found[0], nil
	}
	return ProviderSpec{}, fmt.Errorf("multiple primary providers: %v", providerNames(found))
}

// ProvidersForNode returns the providers configured to install on node,
// filtered against any provider-disabled overlay, the node's enabled
// flag, and the node's tags.
func (c *Config) ProvidersForNode(node string) []ProviderSpec {
	n, err := c.NodeByName(node)
	if err != nil || !n.IsEnabled() {
		return nil
	}
	disabled := map[string]bool{}
	for _, name := range c.ProvidersDisabled {
		disabled[name] = true
	}
	tagSet := map[string]bool{}
	for _, t := range n.Tags {
		tagSet[t] = true
	}
	out := []ProviderSpec{}
	for _, p := range c.Providers {
		if disabled[p.Name] || p.Disabled {
			continue
		}
		if !contains(p.InstallOn, node) {
			continue
		}
		if !satisfiesTags(p.RequiresTags, tagSet) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// EnabledNodes returns coordinator + workers whose Enabled flag is
// nil/true. Use this to drive the per-node loop in Ring 2+ tests.
func (c *Config) EnabledNodes() []NodeSpec {
	out := []NodeSpec{}
	if c.Cluster.Coordinator.IsEnabled() {
		out = append(out, c.Cluster.Coordinator)
	}
	for _, w := range c.Cluster.Workers {
		if w.IsEnabled() {
			out = append(out, w)
		}
	}
	return out
}

// NodeByName returns a copy of the matching node spec. Errors when the
// name resolves to nothing — callers should not silently fall through
// to a zero-value NodeSpec.
func (c *Config) NodeByName(name string) (NodeSpec, error) {
	if c.Cluster.Coordinator.Name == name {
		return c.Cluster.Coordinator, nil
	}
	for _, w := range c.Cluster.Workers {
		if w.Name == name {
			return w, nil
		}
	}
	return NodeSpec{}, fmt.Errorf("no node named %q", name)
}

// NodeTags returns the tags for the named node, or nil if unknown.
func (c *Config) NodeTags(name string) []string {
	if n, err := c.NodeByName(name); err == nil {
		return n.Tags
	}
	return nil
}

func providerNames(p []ProviderSpec) []string {
	out := make([]string, len(p))
	for i, x := range p {
		out[i] = x.Name
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func satisfiesTags(required []string, have map[string]bool) bool {
	for _, t := range required {
		if !have[t] {
			return false
		}
	}
	return true
}
