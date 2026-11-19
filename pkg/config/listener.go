package config

import (
	"fmt"
	"net"
	"regexp"
)

// ListenerConfig identifies effective engine listener settings in resolved launch inputs.
type ListenerConfig struct {
	HostParameter   string `yaml:"host_parameter,omitempty" json:"host_parameter,omitempty"`
	HostEnvironment string `yaml:"host_environment,omitempty" json:"host_environment,omitempty"`
	DefaultHost     string `yaml:"default_host,omitempty" json:"default_host,omitempty"`
}

var listenerEnvironmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
var listenerHostnamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

// Validate rejects listener declarations that cannot identify a bounded host value.
func (l *ListenerConfig) Validate() error {
	if l == nil {
		return nil
	}
	if l.HostParameter == "" && l.HostEnvironment == "" && l.DefaultHost == "" {
		return fmt.Errorf("runtime.listener requires a host parameter, environment variable or default")
	}
	if l.HostParameter != "" && !browserParameterPattern.MatchString(l.HostParameter) {
		return fmt.Errorf("runtime.listener.host_parameter requires a canonical parameter")
	}
	if l.HostEnvironment != "" && !listenerEnvironmentPattern.MatchString(l.HostEnvironment) {
		return fmt.Errorf("runtime.listener.host_environment requires an environment variable name")
	}
	if l.DefaultHost != "" && net.ParseIP(l.DefaultHost) == nil && !listenerHostnamePattern.MatchString(l.DefaultHost) {
		return fmt.Errorf("runtime.listener.default_host requires a plain IP address or hostname")
	}
	return nil
}
