package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// InstallConfig declares managed Python runtimes, shared only at defaults and node tiers.
type InstallConfig struct {
	Runtimes map[string]InstallRecipe `yaml:"runtimes" json:"runtimes"`
}

// InstallRecipe uses pointers so an omitted field inherits and an empty value replaces.
type InstallRecipe struct {
	Package           *string                     `yaml:"package,omitempty" json:"package,omitempty"`
	VersionConstraint *string                     `yaml:"version_constraint,omitempty" json:"version_constraint,omitempty"`
	Indexes           *InstallIndexes             `yaml:"indexes,omitempty" json:"indexes,omitempty"`
	Companions        map[string]InstallCompanion `yaml:"companions,omitempty" json:"companions,omitempty"`
	Verify            *InstallVerify              `yaml:"verify,omitempty" json:"verify,omitempty"`
	StartupImport     *string                     `yaml:"startup_import,omitempty" json:"startup_import,omitempty"`
	OnlyBinary        *bool                       `yaml:"only_binary,omitempty" json:"only_binary,omitempty"`
	Timeout           *string                     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Toolkit           *string                     `yaml:"toolkit,omitempty" json:"toolkit,omitempty"`
}

// InstallIndexes names pip sources; extra is a pointer to preserve an explicit empty array.
type InstallIndexes struct {
	Primary *string   `yaml:"primary,omitempty" json:"primary,omitempty"`
	Extra   *[]string `yaml:"extra,omitempty" json:"extra,omitempty"`
}

// InstallCompanion is another approved root in the same resolver operation.
type InstallCompanion struct {
	Constraint *string `yaml:"constraint,omitempty" json:"constraint,omitempty"`
	Build      *string `yaml:"build,omitempty" json:"build,omitempty"`
}

// InstallVerify adds fixed imports and predicates, never executable source.
type InstallVerify struct {
	Imports *[]string `yaml:"imports,omitempty" json:"imports,omitempty"`
	Checks  *[]string `yaml:"checks,omitempty" json:"checks,omitempty"`
}

// ResolvedInstall carries a detached recipe, provenance, and override ownership.
type ResolvedInstall struct {
	Recipe     InstallRecipe     `json:"recipe"`
	Provenance map[string]string `json:"provenance"`
	Overridden bool              `json:"overridden"`
}

// ResolveInstall merges base, defaults, and the exact node without model tiers.
func (s ServiceConfig) ResolveInstall(node, runtime string) (ResolvedInstall, error) {
	out := ResolvedInstall{Provenance: map[string]string{}}
	merged := map[string]any{}
	tiers := []struct {
		config *InstallConfig
		name   string
	}{{s.Install, "release"}}
	if s.Defaults != nil {
		tiers = append(tiers, struct {
			config *InstallConfig
			name   string
		}{s.Defaults.Install, "defaults"})
	}
	tiers = append(tiers, struct {
		config *InstallConfig
		name   string
	}{s.Nodes[node].Install, "nodes." + node})
	for i, tier := range tiers {
		if tier.config == nil {
			continue
		}
		recipe, ok := tier.config.Runtimes[runtime]
		if !ok {
			continue
		}
		data, err := json.Marshal(recipe)
		if err != nil {
			return out, err
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			return out, err
		}
		if i > 0 && len(fields) > 0 {
			out.Overridden = true
		}
		mergeInstallFields(merged, fields, "", tier.name, out.Provenance)
	}
	if len(merged) == 0 {
		return out, fmt.Errorf("runtime %q not declared for provider %q", runtime, s.Name)
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out.Recipe); err != nil {
		return out, err
	}
	return out, out.Recipe.Validate()
}

func mergeInstallFields(dst, src map[string]any, path, tier string, provenance map[string]string) {
	for key, value := range src {
		leaf := key
		if path != "" {
			leaf = path + "." + key
		}
		if fields, ok := value.(map[string]any); ok {
			existing, _ := dst[key].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			mergeInstallFields(existing, fields, leaf, tier, provenance)
			dst[key] = existing
		} else {
			dst[key] = value
			provenance[leaf] = tier
		}
	}
}

// MergeInstallPatch applies RFC 7396 only to the override, so null restores inheritance.
func MergeInstallPatch(current *InstallConfig, raw []byte) (*InstallConfig, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil {
		return nil, err
	}
	if patch == nil {
		return nil, fmt.Errorf("install must be an object")
	}
	original := map[string]any{}
	if current != nil {
		data, err := json.Marshal(current)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &original); err != nil {
			return nil, err
		}
	}
	mergeInstallPatch(original, patch)
	data, err := json.Marshal(original)
	if err != nil {
		return nil, err
	}
	var result InstallConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Runtimes) == 0 {
		return nil, nil
	}
	return &result, nil
}

func mergeInstallPatch(dst, src map[string]any) {
	for key, value := range src {
		if value == nil {
			delete(dst, key)
			continue
		}
		if fields, ok := value.(map[string]any); ok {
			existing, _ := dst[key].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			mergeInstallPatch(existing, fields)
			dst[key] = existing
		} else {
			dst[key] = value
		}
	}
}

// Validate checks a complete resolved recipe without granting execution authority.
func (r InstallRecipe) Validate() error {
	if r.Package == nil || !ValidDistribution(*r.Package) {
		return fmt.Errorf("package: normalized distribution name required")
	}
	if r.StartupImport == nil || !ValidInstallModule(*r.StartupImport) {
		return fmt.Errorf("startup_import: dotted module required")
	}
	if r.VersionConstraint != nil {
		if err := ValidateSpecifier(*r.VersionConstraint); err != nil {
			return fmt.Errorf("version_constraint: %w", err)
		}
	}
	for _, validate := range []func() error{r.validateIndexes, r.validateCompanions, r.validateChecks} {
		if err := validate(); err != nil {
			return err
		}
	}
	if r.OnlyBinary == nil {
		return fmt.Errorf("only_binary: explicit boolean required")
	}
	if r.Timeout == nil {
		return fmt.Errorf("timeout: explicit duration required")
	}
	duration, err := time.ParseDuration(*r.Timeout)
	if err != nil || duration < time.Minute || duration > 2*time.Hour {
		return fmt.Errorf("timeout: duration between 1m and 2h required")
	}
	if r.Toolkit != nil && *r.Toolkit != "" && *r.Toolkit != "cuda13" {
		return fmt.Errorf("toolkit: unknown managed layout")
	}
	if r.Toolkit != nil && *r.Toolkit != "" && !slices.Contains(*r.Verify.Checks, "managed_toolkit") {
		return fmt.Errorf("verify.checks: managed_toolkit is mandatory for a managed compiler")
	}
	return nil
}

func (r InstallRecipe) validateIndexes() error {
	if r.Indexes == nil || r.Indexes.Primary == nil {
		return fmt.Errorf("indexes.primary: HTTPS index required")
	}
	indexes := []string{*r.Indexes.Primary}
	if r.Indexes.Extra != nil {
		indexes = append(indexes, *r.Indexes.Extra...)
	}
	if len(indexes) > 5 {
		return fmt.Errorf("indexes: at most five sources")
	}
	for _, index := range indexes {
		if err := ValidateInstallIndex(index); err != nil {
			return err
		}
	}
	return nil
}

func (r InstallRecipe) validateCompanions() error {
	if len(r.Companions) > 24 {
		return fmt.Errorf("companions: at most 24 packages")
	}
	for name, c := range r.Companions {
		if !ValidDistribution(name) || name == *r.Package {
			return fmt.Errorf("companions.%s: distinct normalized package required", name)
		}
		if c.Constraint != nil {
			if err := ValidateSpecifier(*c.Constraint); err != nil {
				return fmt.Errorf("companions.%s.constraint: %w", name, err)
			}
		}
		if c.Build == nil || !slices.Contains([]string{"cuda", "metal", "none"}, *c.Build) {
			return fmt.Errorf("companions.%s.build: cuda, metal or none required", name)
		}
	}
	return nil
}

func (r InstallRecipe) validateChecks() error {
	if r.Verify == nil || r.Verify.Imports == nil || r.Verify.Checks == nil {
		return fmt.Errorf("verify: imports and checks required")
	}
	if len(*r.Verify.Imports) > 32 || len(*r.Verify.Checks) > 8 {
		return fmt.Errorf("verify: probe limits exceeded")
	}
	for _, module := range *r.Verify.Imports {
		if !ValidInstallModule(module) {
			return fmt.Errorf("verify.imports: invalid module %q", module)
		}
	}
	if !slices.Contains(*r.Verify.Imports, *r.StartupImport) {
		return fmt.Errorf("verify.imports: required startup import %q missing", *r.StartupImport)
	}
	for _, check := range *r.Verify.Checks {
		if !slices.Contains([]string{"pip_check", "imports", "cuda_companions", "cuda_available", "metal_available", "managed_toolkit"}, check) {
			return fmt.Errorf("verify.checks: unknown check %q", check)
		}
	}
	for _, check := range []string{"pip_check", "imports"} {
		if !slices.Contains(*r.Verify.Checks, check) {
			return fmt.Errorf("verify.checks: %s is mandatory", check)
		}
	}
	for _, c := range r.Companions {
		required := ""
		if *c.Build == "cuda" {
			required = "cuda_companions"
		}
		if *c.Build == "metal" {
			required = "metal_available"
		}
		if required != "" && !slices.Contains(*r.Verify.Checks, required) {
			return fmt.Errorf("verify.checks: %s is mandatory for companion builds", required)
		}
	}
	return nil
}

// ValidDistribution accepts normalized PyPI names, excluding requirement syntax.
func ValidDistribution(value string) bool {
	return len(value) <= 128 && regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`).MatchString(value)
}

// ValidInstallModule accepts bounded dotted Python identifiers.
func ValidInstallModule(value string) bool {
	return len(value) <= 128 && regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`).MatchString(value)
}

// ValidateSpecifier rejects URLs, markers, paths, extras and malformed comparison lists.
func ValidateSpecifier(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 256 {
		return fmt.Errorf("specifier exceeds 256 bytes")
	}
	for _, part := range strings.Split(value, ",") {
		match := regexp.MustCompile(`(?i)^(===|~=|==|!=|<=|>=|<|>)\s*(v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*(?:(?:[-_.]?(?:alpha|beta|preview|pre|a|b|c|rc)[-_.]?[0-9]*))?(?:(?:-[0-9]+)|(?:[-_.]?(?:post|rev|r)[-_.]?[0-9]*))?(?:[-_.]?dev[-_.]?[0-9]*)?(?:\+[a-z0-9]+(?:[._-][a-z0-9]+)*)?(?:\.\*)?)$`).FindStringSubmatch(strings.TrimSpace(part))
		if match == nil {
			return fmt.Errorf("invalid packaging specifier %q", part)
		}
		if strings.HasSuffix(match[2], ".*") && (match[1] != "==" && match[1] != "!=" || !regexp.MustCompile(`^v?(?:[0-9]+!)?[0-9]+(?:\.[0-9]+)*\.\*$`).MatchString(match[2])) {
			return fmt.Errorf("wildcards require == or !=")
		}
		if match[1] == "~=" && !regexp.MustCompile(`(?i)^v?(?:[0-9]+!)?[0-9]+\.[0-9]+`).MatchString(match[2]) {
			return fmt.Errorf("compatible release requires two version segments")
		}
		if strings.Contains(match[2], "+") && match[1] != "==" && match[1] != "!=" && match[1] != "===" {
			return fmt.Errorf("local versions require equality")
		}
	}
	return nil
}

// ValidateInstallIndex validates source syntax; independent authority checks approved origins.
func ValidateInstallIndex(raw string) error {
	return validateInstallURL(raw, false)
}

// ValidateInstallArtifactURL permits escaped wheel names retained only as provenance.
func ValidateInstallArtifactURL(raw string) error {
	return validateInstallURL(raw, true)
}

func validateInstallURL(raw string, escapedPath bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.Port() != "" && u.Port() != "443" || strings.Contains(u.Host, "%") || strings.ContainsAny(raw, "?#\n\r\x00") {
		return fmt.Errorf("indexes: clean HTTPS origin required")
	}
	host := u.Hostname()
	if strings.HasSuffix(u.Host, ":") || strings.HasPrefix(u.Host, "[") && net.ParseIP(host) == nil {
		return fmt.Errorf("indexes: clean HTTPS host required")
	}
	if net.ParseIP(host) == nil && strings.IndexFunc(host, func(r rune) bool {
		return !(installURLAlphaNumeric(r) || r == '.' || r == '-')
	}) >= 0 {
		return fmt.Errorf("indexes: clean HTTPS host required")
	}
	// Indexes enter cmd.exe commands; percent expansion also occurs inside quotes.
	if !escapedPath && strings.Contains(raw, "%") || strings.IndexFunc(u.Path, func(r rune) bool {
		return !(installURLAlphaNumeric(r) || strings.ContainsRune("/._~+-", r))
	}) >= 0 {
		return fmt.Errorf("indexes: unsafe URL path")
	}
	return nil
}

func installURLAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// ValidateInstall validates every effective runtime while allowing irrelevant remote overrides.
func (s ServiceConfig) ValidateInstall() error {
	if s.Install == nil {
		if s.Defaults != nil && s.Defaults.Install != nil {
			return fmt.Errorf("install overrides require declared runtimes")
		}
		for _, n := range s.Nodes {
			if n.Install != nil {
				return fmt.Errorf("install overrides require declared runtimes")
			}
		}
		return nil
	}
	if len(s.Install.Runtimes) == 0 || len(s.Install.Runtimes) > 16 {
		return fmt.Errorf("install: 1 to 16 runtimes required")
	}
	nodes := []string{""}
	for name := range s.Nodes {
		nodes = append(nodes, name)
	}
	for runtime := range s.Install.Runtimes {
		if !ValidDistribution(runtime) {
			return fmt.Errorf("install: invalid runtime ID %q", runtime)
		}
		for _, node := range nodes {
			if _, err := s.ResolveInstall(node, runtime); err != nil {
				return fmt.Errorf("install.runtimes.%s: %w", runtime, err)
			}
		}
	}
	overrides := []*InstallConfig{}
	if s.Defaults != nil {
		overrides = append(overrides, s.Defaults.Install)
	}
	for _, n := range s.Nodes {
		overrides = append(overrides, n.Install)
	}
	for _, override := range overrides {
		if override == nil {
			continue
		}
		for runtime := range override.Runtimes {
			if _, ok := s.Install.Runtimes[runtime]; !ok {
				return fmt.Errorf("install: undeclared runtime %q", runtime)
			}
		}
	}
	return nil
}
