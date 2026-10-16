package schema

import (
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"

	"github.com/stperic/zzrouter/pkg/config/runsettings"

	"gopkg.in/yaml.v3"
)

// YAMLSchema is the on-disk per-provider schema.yaml shape: launcher-
// non-structural params + policy on the structural ones, plus an
// optional per-endpoint overlay schema mirroring config.yaml's
// defaults.endpoints.<E>.parameters tree.
type YAMLSchema struct {
	AutoContext          `yaml:",inline"`
	MemoryUsableFraction *float64                `yaml:"memory_usable_fraction,omitempty"`
	Provider             string                  `yaml:"provider"`
	Parameters           map[string]YAMLParam    `yaml:"parameters,omitempty"`
	Endpoints            map[string]YAMLEndpoint `yaml:"endpoints,omitempty"`
	Diagnostics          *Diagnostics            `yaml:"diagnostics,omitempty"`
}

// YAMLParam mirrors ParamShape on disk. Kind is a string so yaml.v3 decodes
// cleanly; Merge validates it against ParamKind.
type YAMLParam struct {
	AutoContext `yaml:",inline"`
	Type        string   `yaml:"type,omitempty"`
	Min         *float64 `yaml:"min,omitempty"`
	Max         *float64 `yaml:"max,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Enum        []string `yaml:"enum,omitempty"`
	// Pass is how an asset reaches the engine; only type: asset takes it.
	Pass            string            `yaml:"pass,omitempty"`
	Auto            string            `yaml:"auto,omitempty"`
	Served          string            `yaml:"served,omitempty"`
	KVCacheDtype    string            `yaml:"kv_cache_dtype,omitempty"`
	ContextOverride bool              `yaml:"context_override,omitempty"`
	PlacementLog    map[string]string `yaml:"placement_log,omitempty"`
}

// YAMLEndpoint is the on-disk shape for an endpoint overlay schema. Only
// parameters are schema-gated today; environment overlays remain free-form.
type YAMLEndpoint struct {
	Parameters map[string]YAMLParam `yaml:"parameters,omitempty"`
}

// ProviderSchema is the merged Go-spine ∪ schema.yaml result for one
// provider. Endpoints is the per-endpoint overlay schema; absent when
// the provider declares none.
type ProviderSchema struct {
	AutoContext
	MemoryUsableFraction float64
	Parameters           map[string]ParamShape
	Endpoints            map[string]EndpointSchema
	Diagnostics          *Diagnostics
}

// EndpointSchema is the merged shape for one endpoint overlay's params.
type EndpointSchema struct {
	Parameters map[string]ParamShape
}

// ForEndpoint returns the shapes that apply to a launch on endpoint: the
// flat parameters with that endpoint's overlay on top, since an overlay
// may narrow a flat key. An unknown or empty endpoint gets the flat set
// itself, so callers must not modify the result.
func (p *ProviderSchema) ForEndpoint(endpoint string) map[string]ParamShape {
	ep := p.Endpoints[endpoint].Parameters
	if len(ep) == 0 {
		return p.Parameters
	}
	out := make(map[string]ParamShape, len(p.Parameters)+len(ep))
	maps.Copy(out, p.Parameters)
	maps.Copy(out, ep)
	return out
}

// LoadYAMLSchema decodes a schema.yaml payload and returns it. Caller
// validates via Merge; bad YAML is a hard error.
func LoadYAMLSchema(data []byte) (*YAMLSchema, error) {
	var s YAMLSchema
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode schema yaml: %w", err)
	}
	return &s, nil
}

// Merge unions the YAML entries onto the Go spine for one provider. When a
// parameter is declared in both, the YAML's Kind must match the Go Kind —
// otherwise it is a shape conflict and merge fails (plan §4.4). YAML-only
// entries flow through. Go-only entries remain authoritative.
//
// Returns the merged schema plus any conflict errors (collected rather than
// fail-fast so startup reports all of them).
func Merge(provider string, y *YAMLSchema) (*ProviderSchema, []error) {
	out := &ProviderSchema{Parameters: map[string]ParamShape{
		runsettings.KeepAliveParameter: {Kind: ParamString, Description: "Launch idle duration, 0 after release, negative pins against idle expiry and pressure reclaim; inference hints do not change pins"},
		runsettings.EvictIdleParameter: {Kind: ParamBool, Description: "Reclaim unpinned idle runs when this launch cannot fit"},
	}}
	for k, v := range Registry[provider] {
		out.Parameters[k] = v
	}
	var errs []error
	if y == nil {
		return out, nil
	}
	out.AutoContext = y.AutoContext
	if err := y.AutoContext.validate(); err != nil {
		errs = append(errs, err)
	}
	if err := y.Diagnostics.Validate(); err != nil {
		errs = append(errs, err)
	} else {
		out.Diagnostics = y.Diagnostics
	}
	if f := y.MemoryUsableFraction; f != nil {
		if !(*f > 0 && *f <= 1) || out.Diagnostics == nil || out.Diagnostics.Memory == nil || out.Diagnostics.Memory.Kind != MemoryUnified {
			errs = append(errs, fmt.Errorf("memory_usable_fraction requires a unified budget and 0 < fraction <= 1"))
		} else {
			out.MemoryUsableFraction = *f
		}
	}
	for name, yp := range y.Parameters {
		merged, mErr := mergeOne(provider, name, yp, out.Parameters[name])
		if mErr != nil {
			errs = append(errs, mErr)
			continue
		}
		out.Parameters[name] = merged
	}
	if out.Diagnostics != nil && out.Diagnostics.Memory != nil && out.Diagnostics.Memory.Kind == MemoryFractionTotal {
		name := out.Diagnostics.Memory.Parameter
		shape := out.Parameters[name]
		shape.Auto = "auto"
		out.Parameters[name] = shape
	}
	if len(y.Endpoints) > 0 {
		out.Endpoints = make(map[string]EndpointSchema, len(y.Endpoints))
		for epName, ep := range y.Endpoints {
			ps := make(map[string]ParamShape, len(ep.Parameters))
			for name, yp := range ep.Parameters {
				// Endpoint params layer on top of the base; if the same name
				// also exists at flat tier, the Go spine claim still wins on
				// shape. Otherwise treat as a fresh YAML-only declaration.
				base := out.Parameters[name]
				merged, mErr := mergeOne(provider, fmt.Sprintf("endpoints.%s.parameters.%s", epName, name), yp, base)
				if mErr != nil {
					errs = append(errs, mErr)
					continue
				}
				ps[name] = merged
			}
			out.Endpoints[epName] = EndpointSchema{Parameters: ps}
		}
	}
	return out, errs
}

// mergeOne merges one YAML param entry onto a (possibly empty) base shape.
// The base is treated as "Go spine if present" — its non-empty fields win.
func mergeOne(provider, qualifiedName string, yp YAMLParam, base ParamShape) (ParamShape, error) {
	yk, kErr := parseKind(yp.Type)
	if kErr != nil {
		return ParamShape{}, fmt.Errorf("%s.%s: %w", provider, qualifiedName, kErr)
	}
	if base.Kind != "" && yk != "" && base.Kind != yk {
		return ParamShape{}, fmt.Errorf("%s.%s: schema.yaml type %q conflicts with Go spine kind %q",
			provider, qualifiedName, yk, base.Kind)
	}
	out := base
	if out.Kind == "" {
		out.Kind = yk
	}
	if out.Min == nil {
		out.Min = yp.Min
	}
	if out.Max == nil {
		out.Max = yp.Max
	}
	if out.Description == "" {
		out.Description = yp.Description
	}
	if len(out.Enum) == 0 {
		out.Enum = yp.Enum
	}
	pass, err := parsePass(yp.Pass, out.Kind)
	if err != nil {
		return ParamShape{}, fmt.Errorf("%s.%s: %w", provider, qualifiedName, err)
	}
	if out.Pass == "" {
		out.Pass = pass
	}
	if out.Auto == "" {
		out.Auto = yp.Auto
	}
	if yp.AutoMinContextTokens != 0 || yp.AutoContextSource != "" {
		out.AutoContext = yp.AutoContext
	}
	if err := out.AutoContext.validate(); err != nil || ((out.AutoMinContextTokens != 0 || out.AutoContextSource != "") && (out.Kind != ParamInt || out.Auto == "")) {
		return ParamShape{}, fmt.Errorf("%s.%s: auto context policy requires an auto integer and valid metadata", provider, qualifiedName)
	}
	if out.Served == "" {
		out.Served = yp.Served
	}
	if yp.KVCacheDtype != "" {
		out.KVCacheDtype = yp.KVCacheDtype
	}
	switch out.KVCacheDtype {
	case "", "k", "v", "both":
	default:
		return ParamShape{}, fmt.Errorf("%s.%s: invalid kv_cache_dtype role", provider, qualifiedName)
	}
	out.ContextOverride = out.ContextOverride || yp.ContextOverride
	if yp.PlacementLog != nil {
		out.PlacementLog = make(map[string]*regexp.Regexp, len(yp.PlacementLog))
		for name, raw := range yp.PlacementLog {
			groups, ok := map[string]int{"layers": 2, "cpu_overrides": 0, "weights": 2}[name]
			if !ok {
				return ParamShape{}, fmt.Errorf("%s.%s: unknown placement_log.%s", provider, qualifiedName, name)
			}
			pattern, err := regexp.Compile(raw)
			if err != nil || pattern.NumSubexp() != groups {
				return ParamShape{}, fmt.Errorf("%s.%s: placement_log.%s requires a valid regex with exactly %d capture groups", provider, qualifiedName, name, groups)
			}
			out.PlacementLog[name] = pattern
		}
	}
	return out, nil
}

// parsePass validates an asset's pass mode. It means nothing for any
// other kind, so it is refused there rather than ignored.
func parsePass(s string, kind ParamKind) (AssetPass, error) {
	switch {
	case s == "":
		return "", nil
	case kind != ParamAsset:
		return "", fmt.Errorf("pass %q applies only to type asset, not %q", s, kind)
	case s == string(AssetPassPath), s == string(AssetPassContent):
		return AssetPass(s), nil
	default:
		return "", fmt.Errorf("unknown pass %q (want %s or %s)", s, AssetPassPath, AssetPassContent)
	}
}

func parseKind(s string) (ParamKind, error) {
	switch s {
	case "":
		return "", nil
	case string(ParamInt), string(ParamFloat), string(ParamString), string(ParamBool), string(ParamAsset):
		return ParamKind(s), nil
	default:
		return "", fmt.Errorf("unknown type %q", s)
	}
}

// LoadFromFS reads every providers/<kind>/<name>/schema.yaml under root
// and returns merged schemas keyed by provider name. Only that exact
// depth is read: a deeper file, such as an operator's asset named
// schema.yaml, is never a schema. Providers without one get the Go spine.
func LoadFromFS(fsys fs.FS, root string) (map[string]*ProviderSchema, []error) {
	merged := make(map[string]*ProviderSchema)
	for provider := range Registry {
		merged[provider] = &ProviderSchema{Parameters: cloneProvider(Registry[provider])}
	}
	paths, err := fs.Glob(fsys, path.Join(root, "*", "*", schemaFileName))
	if err != nil {
		return merged, []error{fmt.Errorf("find schema files: %w", err)}
	}
	var errs []error
	for _, p := range paths {
		provider := path.Base(path.Dir(p))
		data, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", p, rerr))
			continue
		}
		y, perr := LoadYAMLSchema(data)
		if perr != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", p, perr))
			continue
		}
		m, mergeErrs := Merge(provider, y)
		errs = append(errs, mergeErrs...)
		merged[provider] = m
	}
	return merged, errs
}

// schemaFileName is the per-provider schema beside its config.yaml.
const schemaFileName = "schema.yaml"

func cloneProvider(m map[string]ParamShape) map[string]ParamShape {
	return maps.Clone(m)
}
