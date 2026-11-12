package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ConnectionConfig declares input fields for a provider's upstream connection.
type ConnectionConfig struct {
	Fields   []ConnectionField `yaml:"fields" json:"fields"`
	TestPath string            `yaml:"test_path,omitempty" json:"test_path,omitempty"`
}

// ConnectionField is a declared value, never a stored credential.
type ConnectionField struct {
	ID           string   `yaml:"id" json:"id"`
	Label        string   `yaml:"label" json:"label"`
	Type         string   `yaml:"type" json:"type"`
	Secret       bool     `yaml:"secret,omitempty" json:"secret"`
	Required     bool     `yaml:"required,omitempty" json:"required"`
	Env          string   `yaml:"env" json:"env"`
	EnvAliases   []string `yaml:"env_aliases,omitempty" json:"env_aliases,omitempty"`
	Default      string   `yaml:"default,omitempty" json:"default,omitempty"`
	FallbackFile string   `yaml:"fallback_file,omitempty" json:"-"`
}

var connectionIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks declarations before they become available to clients.
func (c *ConnectionConfig) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Fields) == 0 || len(c.Fields) > 16 {
		return fmt.Errorf("connection requires between 1 and 16 fields")
	}
	ids, envs := map[string]bool{}, map[string]bool{}
	for _, f := range c.Fields {
		if strings.HasPrefix(f.ID, "_") || !connectionIdentifier.MatchString(f.ID) || !connectionIdentifier.MatchString(f.Env) || f.Label == "" || ids[f.ID] || envs[f.Env] {
			return fmt.Errorf("connection has invalid or duplicate field %q", f.ID)
		}
		ids[f.ID], envs[f.Env] = true, true
		if f.Type != "text" && f.Type != "endpoint" && f.Type != "identifier" {
			return fmt.Errorf("connection field %q has invalid type", f.ID)
		}
		if f.Secret && f.Default != "" {
			return fmt.Errorf("connection field %q cannot have a secret default", f.ID)
		}
		for _, alias := range f.EnvAliases {
			if !connectionIdentifier.MatchString(alias) || envs[alias] {
				return fmt.Errorf("connection field %q has invalid environment alias", f.ID)
			}
			envs[alias] = true
		}
		if err := f.ValidateValue(f.Default, false); err != nil {
			return err
		}
	}
	if c.TestPath != "" && (!strings.HasPrefix(c.TestPath, "/") || strings.HasPrefix(c.TestPath, "//")) {
		return fmt.Errorf("connection test_path must be a relative absolute path")
	}
	return nil
}

// ValidateValue validates a candidate without including its value in errors.
func (f ConnectionField) ValidateValue(value string, required bool) error {
	if len(value) > 16<<10 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("connection field %q has an invalid value", f.ID)
	}
	if value == "" {
		if required && f.Required && f.Default == "" {
			return fmt.Errorf("connection field %q is required", f.ID)
		}
		return nil
	}
	switch f.Type {
	case "identifier":
		for _, r := range value {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return fmt.Errorf("connection field %q must be an identifier", f.ID)
			}
		}
	case "endpoint":
		if !ValidConnectionEndpoint(value) {
			return fmt.Errorf("connection field %q must be an HTTPS endpoint (HTTP is allowed on loopback)", f.ID)
		}
	}
	return nil
}

// ValidConnectionEndpoint excludes credential-bearing URLs and cleartext remote hosts.
func ValidConnectionEndpoint(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}
