package config

import (
	"fmt"
	"regexp"
	"strings"
)

// InstallSource pins a release-owned source tree for a managed Python runtime.
type InstallSource struct {
	Version         string `yaml:"version" json:"version"`
	URL             string `yaml:"url" json:"url"`
	SHA256          string `yaml:"sha256" json:"sha256"`
	StripComponents int    `yaml:"strip_components" json:"strip_components"`
	Requirements    string `yaml:"requirements" json:"requirements"`
	Entrypoint      string `yaml:"entrypoint" json:"entrypoint"`
}

var (
	sourceVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	sourceSHA256Pattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func (s InstallSource) validate() error {
	if len(s.Version) > 128 || !sourceVersionPattern.MatchString(s.Version) {
		return fmt.Errorf("source.version: bounded release identity required")
	}
	if err := ValidateInstallIndex(s.URL); err != nil {
		return fmt.Errorf("source.url: %w", err)
	}
	if !strings.HasSuffix(s.URL, ".tar.gz") {
		return fmt.Errorf("source.url: tar.gz release archive required")
	}
	if !sourceSHA256Pattern.MatchString(s.SHA256) {
		return fmt.Errorf("source.sha256: lowercase SHA256 required")
	}
	if s.StripComponents != 1 {
		return fmt.Errorf("source.strip_components: must be 1")
	}
	for name, file := range map[string]string{"requirements": s.Requirements, "entrypoint": s.Entrypoint} {
		if len(file) > 256 || !validProviderDirectory(file) {
			return fmt.Errorf("source.%s: contained relative file required", name)
		}
	}
	return nil
}

func (s ServiceConfig) validateSourceExecution(runtime string, source *InstallSource) error {
	if runtime == s.Name && s.VersionSource != nil {
		return fmt.Errorf("source: version_source is forbidden; source.version is the release pin")
	}
	execution := s.RuntimeExecution(runtime)
	if execution == nil || execution.Type != "python" || len(execution.Args) == 0 || execution.Args[0] != source.Entrypoint {
		return fmt.Errorf("source.entrypoint: must match the Python execution's first argument")
	}
	dir := strings.ToLower(execution.WorkingDir)
	if !validProviderDirectory(dir) {
		return fmt.Errorf("source: contained execution.working_dir required")
	}
	reserved := []string{SubdirBin, SubdirVenv, SubdirCandidate, SubdirPrevious, ProviderManagedName, ProviderVersionName, ProviderManifestName, ProviderManifestTemp, SubdirPython}
	if s.Runtime != nil && s.Runtime.DataDir != "" {
		reserved = append(reserved, strings.ToLower(s.Runtime.DataDir))
	}
	for _, other := range reserved {
		if dir == other || strings.HasPrefix(dir, other+"/") || strings.HasPrefix(other, dir+"/") {
			return fmt.Errorf("source: execution.working_dir overlaps reserved path %q", other)
		}
	}
	return nil
}

// RuntimeExecution selects the configured base or feature execution for a runtime.
func (s ServiceConfig) RuntimeExecution(runtime string) *ExecutionConfig {
	var execution *ExecutionConfig
	if runtime == s.Name && s.Runtime != nil {
		execution = &s.Runtime.Execution
	}
	for _, feature := range s.Features {
		if feature.Runtime == runtime {
			execution = feature.Execution
		}
	}
	return execution
}
