package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
	"github.com/stperic/zzrouter/pkg/prov_apps/upstream"
	"github.com/stperic/zzrouter/pkg/security"
	"gopkg.in/yaml.v3"
)

// RecipeSnapshot binds a resolved runtime to independently checked authority.
type RecipeSnapshot struct {
	Recheck func(context.Context) error `json:"-"`
	config.ResolvedInstall
	Provider           string                   `json:"provider"`
	Runtime            string                   `json:"runtime"`
	Node               string                   `json:"node"`
	Fingerprint        string                   `json:"fingerprint"`
	PolicyFingerprint  string                   `json:"policy_fingerprint"`
	Authority          string                   `json:"authority"`
	Policy             *RuntimePolicy           `json:"-"`
	AdditionalPackages map[string]string        `json:"additional_packages,omitempty"`
	Requirements       *config.AppRequirements  `json:"-"`
	PinnedVersion      string                   `json:"-"`
	WorkingDir         string                   `json:"-"`
	DataRuntime        *config.AppRuntimeConfig `json:"-"`
}

// PayloadDirectories lists the owned runtime trees in activation order.
func (s RecipeSnapshot) PayloadDirectories() []string {
	if s.Recipe.Source != nil {
		return []string{s.WorkingDir, config.SubdirVenv}
	}
	return []string{config.SubdirVenv}
}

// RecipeResolver reads current config and authority on the executing node.
type RecipeResolver func(provider, runtime string) (RecipeSnapshot, error)

// InstallPolicy is startup-only operator authority, never written by sync or API.
type InstallPolicy struct {
	Runtimes map[string]RuntimePolicy `yaml:"runtimes" json:"runtimes"`
}

// RuntimePolicy grants bounded pip overrides or additive roots for released source.
type RuntimePolicy struct {
	AdditionalPackages  map[string]string `yaml:"additional_packages,omitempty" json:"additional_packages,omitempty"`
	Packages            map[string]string `yaml:"packages" json:"packages"`
	Imports             []string          `yaml:"imports" json:"imports"`
	Entrypoints         []string          `yaml:"entrypoints" json:"entrypoints"`
	Indexes             []string          `yaml:"indexes" json:"indexes"`
	AllowSourceBuilds   bool              `yaml:"allow_source_builds" json:"allow_source_builds"`
	AllowPrivateIndexes bool              `yaml:"allow_private_indexes" json:"allow_private_indexes"`
}

// AdditionalPackagesLimit bounds the operator-selected roots of a source runtime.
const AdditionalPackagesLimit = 128

// ErrInstallPolicy distinguishes authority refusal from recipe syntax errors.
var ErrInstallPolicy = fmt.Errorf("protected install policy required")

// ResolveRecipe validates syntax and checks release templates or protected node policy.
func ResolveRecipe(sc config.ServiceConfig, node, runtime, policyPath string, installed *fsroot.InstallManifest) (RecipeSnapshot, error) {
	resolved, err := sc.ResolveInstall(node, runtime)
	if err != nil {
		return RecipeSnapshot{}, err
	}
	out := RecipeSnapshot{ResolvedInstall: resolved, Provider: sc.Name, Runtime: runtime, Node: node, Requirements: sc.Requirements}
	if resolved.Recipe.PinnedVersion != nil {
		out.PinnedVersion = *resolved.Recipe.PinnedVersion
	}
	if resolved.Recipe.Source != nil {
		out.PinnedVersion = resolved.Recipe.Source.Version
		out.WorkingDir = sc.RuntimeExecution(runtime).WorkingDir
		out.DataRuntime = sc.Runtime
		if installed != nil {
			out.AdditionalPackages = maps.Clone(installed.AdditionalPackages)
		}
	} else if err := validateServingImport(sc, runtime, *resolved.Recipe.StartupImport); err != nil {
		return out, err
	}
	if resolved.Recipe.Source == nil && runtime == sc.Name {
		out.PinnedVersion = sc.PinnedVersion
		if sc.VersionSource == nil || sc.VersionSource.Type != config.VersionSourcePyPI || sc.VersionSource.Package != *resolved.Recipe.Package {
			return out, fmt.Errorf("base recipe package must match version_source.package")
		}
	}
	data, readErr := templates.AppsFS.ReadFile("files/providers/on-demand/" + sc.Name + "/config.yaml")
	var shipped config.OnDemandProvider
	if readErr == nil {
		readErr = yaml.Unmarshal(data, &shipped)
	}
	trusted := false
	if readErr == nil && shipped.Install != nil {
		release, ok := shipped.Install.Runtimes[runtime]
		trusted = ok && !resolved.Overridden && reflect.DeepEqual(releaseRecipeValue(resolved.Recipe), releaseRecipeValue(release))
		if ok {
			for _, module := range *release.Verify.Imports {
				if !slices.Contains(*resolved.Recipe.Verify.Imports, module) {
					return out, fmt.Errorf("required startup import %q cannot be removed", module)
				}
			}
			for _, check := range *release.Verify.Checks {
				if !slices.Contains(*resolved.Recipe.Verify.Checks, check) {
					return out, fmt.Errorf("required check %q cannot be removed", check)
				}
			}
		}
	}
	if trusted {
		out.Authority = "release"
	} else {
		if resolved.Recipe.Source != nil {
			return out, fmt.Errorf("%w: source recipes require an unchanged shipped declaration", ErrInstallPolicy)
		}
		out.Authority = "operator"
	}
	out.Fingerprint = RecipeFingerprint(resolved.Recipe)
	if err := out.bindPolicy(policyPath, installed); err != nil {
		return out, err
	}
	return out, nil
}

func (s *RecipeSnapshot) bindPolicy(policyPath string, installed *fsroot.InstallManifest) error {
	var inventory []ResolvedPackage
	if installed != nil {
		inventory = append([]ResolvedPackage{}, installed.Inventory...)
	}
	if s.Authority == "operator" || s.Recipe.Source != nil && (policyPath != "" || len(s.AdditionalPackages) > 0) {
		policy, fp, err := ReadInstallPolicy(policyPath)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInstallPolicy, err)
		}
		grant := policy.Runtimes[s.Provider+"/"+s.Runtime]
		if s.Recipe.Source != nil && installed == nil {
			s.AdditionalPackages = maps.Clone(grant.AdditionalPackages)
		}
		if err := grant.Authorize(*s, inventory); err != nil {
			return fmt.Errorf("%w: %v", ErrInstallPolicy, err)
		}
		s.Policy = &grant
		s.PolicyFingerprint = fp
	}
	if s.PolicyFingerprint == "" {
		s.Recheck = func(ctx context.Context) error { return ctx.Err() }
	} else {
		captured := *s
		captured.AdditionalPackages = maps.Clone(s.AdditionalPackages)
		s.Recheck = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			policy, fp, err := ReadInstallPolicy(policyPath)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInstallPolicy, err)
			}
			return captured.recheckPolicy(policy, fp, inventory)
		}
	}
	return nil
}

func (s RecipeSnapshot) recheckPolicy(policy InstallPolicy, fingerprint string, inventory []ResolvedPackage) error {
	if fingerprint != s.PolicyFingerprint {
		return fmt.Errorf("%w: policy changed; preview again", ErrInstallPolicy)
	}
	return policy.Runtimes[s.Provider+"/"+s.Runtime].Authorize(s, inventory)
}

func releaseRecipeValue(recipe config.InstallRecipe) config.InstallRecipe {
	if len(recipe.Companions) == 0 {
		recipe.Companions = nil
	}
	if recipe.Indexes != nil {
		indexes := *recipe.Indexes
		if indexes.Extra != nil && len(*indexes.Extra) == 0 {
			indexes.Extra = nil
		}
		recipe.Indexes = &indexes
	}
	return recipe
}

func validateServingImport(sc config.ServiceConfig, runtime, module string) error {
	execution := sc.RuntimeExecution(runtime)
	if execution == nil || execution.Type != "python" {
		return nil
	}
	for index, arg := range execution.Args {
		if arg == "-m" && index+1 < len(execution.Args) {
			serving := execution.Args[index+1]
			if index+2 < len(execution.Args) && execution.Args[index+2] == "server" {
				serving += ".server"
			}
			if serving != module {
				return fmt.Errorf("startup_import must match serving module %q", serving)
			}
			return nil
		}
	}
	return fmt.Errorf("python runtime must declare its serving module with -m")
}

// RecipeFingerprint excludes the fresh-install seed, which does not change an installed runtime.
func RecipeFingerprint(recipe config.InstallRecipe) string {
	recipe.PinnedVersion = nil
	return Fingerprint(recipe)
}

// Fingerprint hashes a canonical JSON value without retaining its raw data.
func Fingerprint(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadInstallPolicy reads a bounded, protected file without following symlinks.
func ReadInstallPolicy(path string) (InstallPolicy, string, error) {
	if path == "" {
		return InstallPolicy{}, "", errors.New("no install policy configured on this node: a host administrator " +
			"must install a protected policy file ('zzrouter-node install-policy template' prints one granting the shipped recipes) " +
			"and set providers.install_policy_file in node.yaml, then restart the node; see docs/typed_provider_recipes.md")
	}
	if !filepath.IsAbs(path) {
		return InstallPolicy{}, "", fmt.Errorf("absolute policy path required")
	}
	before, err := checkPolicyPath(path)
	if err != nil {
		return InstallPolicy{}, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return InstallPolicy{}, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return InstallPolicy{}, "", fmt.Errorf("policy replaced during read")
	}
	data, err := io.ReadAll(io.LimitReader(file, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return InstallPolicy{}, "", fmt.Errorf("policy read exceeds bound")
	}
	after, err := checkPolicyPath(path)
	if err != nil || !os.SameFile(opened, after) {
		return InstallPolicy{}, "", fmt.Errorf("policy replaced during read")
	}
	return parseInstallPolicy(data)
}

func parseInstallPolicy(data []byte) (InstallPolicy, string, error) {
	var policy InstallPolicy
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&policy); err != nil {
		return policy, "", err
	}
	if len(policy.Runtimes) == 0 || len(policy.Runtimes) > 32 {
		return policy, "", fmt.Errorf("policy requires 1 to 32 grants")
	}
	for _, grant := range policy.Runtimes {
		packages, limit := grant.Packages, 32
		if grant.AdditionalPackages != nil {
			if grant.Packages != nil || grant.Imports != nil || grant.Entrypoints != nil || grant.Indexes != nil || grant.AllowSourceBuilds || grant.AllowPrivateIndexes {
				return policy, "", fmt.Errorf("additional_packages cannot be mixed with pip override grants")
			}
			packages, limit = grant.AdditionalPackages, AdditionalPackagesLimit
		} else if len(packages) == 0 {
			return policy, "", fmt.Errorf("policy requires package grants or additional_packages")
		}
		if len(grant.Imports) > 64 || len(grant.Indexes) > 5 {
			return policy, "", fmt.Errorf("policy grant exceeds bounds")
		}
		if err := ValidatePackageConstraints(packages, limit); err != nil {
			return policy, "", err
		}
		for _, module := range append(slices.Clone(grant.Imports), grant.Entrypoints...) {
			if !config.ValidInstallModule(module) {
				return policy, "", fmt.Errorf("invalid policy module")
			}
		}
		for _, index := range grant.Indexes {
			if err := validateRecipeIndex(index, grant.AllowPrivateIndexes); err != nil {
				return policy, "", err
			}
		}
	}
	return policy, Fingerprint(policy), nil
}

// ValidatePackageConstraints checks policy and recorded root constraints identically.
func ValidatePackageConstraints(packages map[string]string, limit int) error {
	if len(packages) > limit {
		return fmt.Errorf("package set exceeds %d roots", limit)
	}
	for name, constraint := range packages {
		if !config.ValidDistribution(name) {
			return fmt.Errorf("invalid policy package %q", name)
		}
		if err := config.ValidateSpecifier(constraint); err != nil {
			return err
		}
	}
	return nil
}

// Authorize checks additive or pip roots, and actual versions when inventory is supplied.
func (p RuntimePolicy) Authorize(snapshot RecipeSnapshot, inventory []ResolvedPackage) error {
	recipe := snapshot.Recipe
	grants, roots := p.Packages, map[string]string{}
	if recipe.Source != nil {
		if snapshot.Authority != "release" || p.Packages != nil {
			return fmt.Errorf("source recipes are release-owned")
		}
		grants, roots = p.AdditionalPackages, snapshot.AdditionalPackages
		if err := ValidatePackageConstraints(roots, AdditionalPackagesLimit); err != nil {
			return fmt.Errorf("%w: %v", ErrInstallPolicy, err)
		}
	} else {
		if p.AdditionalPackages != nil {
			return fmt.Errorf("additional_packages requires a release-owned source runtime")
		}
		roots[*recipe.Package] = ""
		for name := range recipe.Companions {
			roots[name] = ""
		}
	}
	for name, constraint := range roots {
		approved, ok := grants[name]
		if !ok {
			return fmt.Errorf("%w: package %q unapproved", ErrInstallPolicy, name)
		}
		if inventory == nil {
			continue
		}
		present := false
		for _, entry := range inventory {
			if entry.Name != name || !entry.Requested {
				continue
			}
			if err := upstream.VersionAllowed(entry.Version, strings.Trim(constraint+","+approved, ",")); err != nil {
				return fmt.Errorf("%w: installed root %s: %v", ErrInstallPolicy, name, err)
			}
			present = true
		}
		if !present {
			return fmt.Errorf("%w: installed root %s lacks requested inventory", ErrInstallPolicy, name)
		}
	}
	if recipe.Source != nil {
		return nil
	}
	for _, entry := range inventory {
		if _, declared := roots[entry.Name]; entry.Requested && !declared {
			return fmt.Errorf("%w: installed root %s is not granted", ErrInstallPolicy, entry.Name)
		}
	}
	if !slices.Contains(p.Entrypoints, *recipe.StartupImport) {
		return fmt.Errorf("startup entrypoint unapproved")
	}
	for _, module := range *recipe.Verify.Imports {
		if !slices.Contains(p.Imports, module) {
			return fmt.Errorf("import %q unapproved", module)
		}
	}
	indexes := []string{*recipe.Indexes.Primary}
	if recipe.Indexes.Extra != nil {
		indexes = append(indexes, *recipe.Indexes.Extra...)
	}
	for _, index := range indexes {
		if !slices.Contains(p.Indexes, index) {
			return fmt.Errorf("index %q unapproved", index)
		}
		if err := validateRecipeIndex(index, p.AllowPrivateIndexes); err != nil {
			return err
		}
	}
	if !*recipe.OnlyBinary && !p.AllowSourceBuilds {
		return fmt.Errorf("source builds unapproved")
	}
	return nil
}

func validateRecipeIndex(raw string, private bool) error {
	if err := config.ValidateInstallIndex(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	if err := security.ValidateDownloadURL(raw, []string{u.Host}); err != nil {
		return err
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if !private && (ip != nil || host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local")) {
		return fmt.Errorf("private or literal index requires operator approval")
	}
	return nil
}

// ValidateRecipeSources checks configured origins again on the executing node.
// pip artifact redirects still require an operator-controlled egress boundary.
func ValidateRecipeSources(ctx context.Context, snapshot RecipeSnapshot) error {
	private := snapshot.Policy != nil && snapshot.Policy.AllowPrivateIndexes
	indexes := []string{*snapshot.Recipe.Indexes.Primary}
	if snapshot.Recipe.Indexes.Extra != nil {
		indexes = append(indexes, *snapshot.Recipe.Indexes.Extra...)
	}
	for _, index := range indexes {
		if err := validateRecipeIndex(index, private); err != nil {
			return err
		}
		if private {
			continue
		}
		u, _ := url.Parse(index)
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
		if err != nil {
			return fmt.Errorf("resolve approved index: %w", err)
		}
		if len(addresses) == 0 {
			return fmt.Errorf("approved index resolved no addresses")
		}
		for _, address := range addresses {
			ip := address.IP
			if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("private index target requires operator approval")
			}
		}
	}
	return nil
}

// RuntimeChecks extends the release probes with the accepted recipe's mandatory checks.
func (s RecipeSnapshot) RuntimeChecks(base schema.RuntimeChecks) schema.RuntimeChecks {
	base.Imports = slices.Clone(*s.Recipe.Verify.Imports)
	base.Checks = slices.Clone(*s.Recipe.Verify.Checks)
	base.Packages = nil
	if s.Recipe.Package != nil {
		base.Packages = append(base.Packages, *s.Recipe.Package)
	}
	for name := range s.Recipe.Companions {
		base.Packages = append(base.Packages, name)
	}
	if _, added := s.AdditionalPackages["torchaudio"]; added && !slices.Contains(base.Packages, "torchaudio") {
		base.Packages = append(base.Packages, "torchaudio")
	}
	slices.Sort(base.Packages)
	return base
}

// CheckRecipeAuthority is repeated before commands, including guided execution.
func CheckRecipeAuthority(ctx context.Context, snapshot RecipeSnapshot, resolver RecipeResolver) error {
	if snapshot.Recheck != nil {
		return snapshot.Recheck(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fresh, err := resolver(snapshot.Provider, snapshot.Runtime)
	if err != nil {
		return err
	}
	if fresh.PolicyFingerprint != snapshot.PolicyFingerprint {
		return fmt.Errorf("%w: policy changed; preview again", ErrInstallPolicy)
	}
	return nil
}
