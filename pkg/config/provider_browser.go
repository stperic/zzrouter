package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ProviderBrowser declares the launch contract required for shared native browser access.
type ProviderBrowser struct {
	Name               string            `yaml:"name" json:"name"`
	RequiredParameters map[string]string `yaml:"required_parameters" json:"required_parameters"`
}

var browserParameterPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,127}$`)

func (p *OnDemandProvider) validateBrowser() error {
	browser := p.Runtime.Browser
	if browser == nil {
		return nil
	}
	if p.RunScope != RunScopeNode || p.Install == nil || p.Install.Runtimes[p.Name].Source == nil {
		return fmt.Errorf("runtime.browser requires a node-scoped source runtime")
	}
	if strings.TrimSpace(browser.Name) == "" || len(browser.Name) > 80 || strings.ContainsAny(browser.Name, "\r\n\x00") {
		return fmt.Errorf("runtime.browser.name: bounded single-line name required")
	}
	if len(browser.RequiredParameters) == 0 || len(browser.RequiredParameters) > 16 {
		return fmt.Errorf("runtime.browser.required_parameters: one to 16 canonical parameters required")
	}
	for key, value := range browser.RequiredParameters {
		if !browserParameterPattern.MatchString(key) || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("runtime.browser.required_parameters: invalid parameter %q", key)
		}
	}
	return nil
}
