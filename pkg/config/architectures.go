package config

import (
	"fmt"
	"slices"
)

// FilterArchitectures selects from the full format-eligible set before name selection.
// Unknown architectures admit declarations only for explicit choices or storage.
func FilterArchitectures(candidates map[string][]string, architecture string, allowUnknown bool) []string {
	declared := false
	for _, accepts := range candidates {
		declared = declared || (architecture != "" && slices.Contains(accepts, architecture))
	}
	var names []string
	for name, accepts := range candidates {
		if (architecture == "" && (allowUnknown || len(accepts) == 0)) ||
			(architecture != "" && (slices.Contains(accepts, architecture) || (!declared && len(accepts) == 0))) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// ValidateArchitectures requires format evidence alongside an architecture declaration.
func (c *AppCapabilities) ValidateArchitectures() error {
	if c != nil && len(c.Architectures) > 0 && len(c.Formats) == 0 {
		return fmt.Errorf("capabilities.architectures requires formats")
	}
	return nil
}
