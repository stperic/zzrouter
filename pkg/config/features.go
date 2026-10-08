package config

import (
	"fmt"
	"path"

	"gopkg.in/yaml.v3"
)

// Feature declares how a provider supplies a model ability. Empty declarations
// describe built-in support; files are tried in preference order.
type Feature struct {
	Default           bool             `yaml:"default,omitempty" json:"default,omitempty"`
	Files             []string         `yaml:"files,omitempty" json:"files,omitempty"`
	Flag              string           `yaml:"flag,omitempty" json:"flag,omitempty"`
	When              string           `yaml:"when,omitempty" json:"when,omitempty"`
	Runtime           string           `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Execution         *ExecutionConfig `yaml:"execution,omitempty" json:"execution,omitempty"`
	WireEndpoints     []string         `yaml:"wire_endpoints,omitempty" json:"wire_endpoints,omitempty"`
	ExcludeParameters []string         `yaml:"exclude_parameters,omitempty" json:"exclude_parameters,omitempty"`
}

func validateFeatures(features map[string]Feature) error {
	for name, f := range features {
		if name != "vision" {
			return fmt.Errorf("feature %q: unknown name", name)
		}
		if len(f.Files) > 0 && f.Runtime != "" {
			return fmt.Errorf("feature %q: files and runtime are mutually exclusive", name)
		}
		if len(f.Files) > 0 && f.Flag == "" {
			return fmt.Errorf("feature %q: files require a flag", name)
		}
		if f.Flag != "" && len(f.Files) == 0 {
			return fmt.Errorf("feature %q: flag requires files", name)
		}
		if f.When != "" && f.Runtime == "" {
			return fmt.Errorf("feature %q: when requires a runtime", name)
		}
		if f.Default && len(f.Files) == 0 && f.Runtime == "" {
			return fmt.Errorf("feature %q: default requires files or runtime", name)
		}
		if f.Runtime == "" && (f.Execution != nil || len(f.WireEndpoints) > 0 || len(f.ExcludeParameters) > 0) {
			return fmt.Errorf("feature %q: execution overlay requires a runtime", name)
		}
		if f.Runtime != "" {
			if err := validateFeatureRuntime(name, f); err != nil {
				return err
			}
		}
		for _, glob := range f.Files {
			if _, err := path.Match(glob, ""); err != nil {
				return fmt.Errorf("feature %q: invalid file glob %q: %w", name, glob, err)
			}
		}
	}
	return nil
}

func validateFeatureRuntime(name string, f Feature) error {
	if f.Execution == nil || f.Execution.Command == "" || (f.Execution.Type != "python" && f.Execution.Type != "cli") {
		return fmt.Errorf("feature %q: runtime requires a cli or python execution command", name)
	}
	if f.When != "" && f.When != "vision_config" {
		return fmt.Errorf("feature %q: unknown predicate %q", name, f.When)
	}
	if v := f.Execution.WireModel; v != "" && v != WireModelName && v != WireModelPath {
		return fmt.Errorf("feature %q: unknown wire_model %q", name, v)
	}
	if err := (&AppCapabilities{WireEndpoints: f.WireEndpoints}).ValidateWireEndpoints(); err != nil {
		return fmt.Errorf("feature %q: %w", name, err)
	}
	return nil
}

// marshalFeatureProvider preserves an explicit empty declaration so reconciliation
// does not mistake a saved opt-out for a config predating features.
func marshalFeatureProvider(provider any, features map[string]Feature) (any, error) {
	var node yaml.Node
	if err := node.Encode(provider); err != nil {
		return nil, err
	}
	if features != nil && len(features) == 0 {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "features"},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle},
		)
	}
	return &node, nil
}
