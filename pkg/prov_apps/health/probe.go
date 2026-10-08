package health

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ReadinessProbe defines how to determine if a process is ready.
type ReadinessProbe struct {
	LogPatterns LogPatterns   `yaml:"logPatterns" json:"log_patterns"`
	Timeout     time.Duration `yaml:"timeout" json:"timeout"`

	// Serve, when set, is the authority on readiness: log patterns say the
	// process started, this says it can serve. Engines that open their
	// socket before loading weights (mlx_lm) are ready only by this test.
	Serve *ServeCheck `yaml:"serve,omitempty" json:"serve,omitempty"`
}

// ServeCheck is a request that succeeds only once the engine can serve
// inference. ${WIRE_MODEL} in Body expands to the token the engine keys on.
type ServeCheck struct {
	Path   string `yaml:"path" json:"path"`
	Method string `yaml:"method,omitempty" json:"method,omitempty"`
	Body   string `yaml:"body,omitempty" json:"body,omitempty"`
}

// Method resolves the HTTP method: POST when a body is declared, else GET.
func (sc *ServeCheck) method() string {
	switch {
	case sc.Method != "":
		return sc.Method
	case sc.Body != "":
		return http.MethodPost
	default:
		return http.MethodGet
	}
}

// Validate checks probe configuration.
func (rp *ReadinessProbe) Validate() error {
	if rp == nil {
		return fmt.Errorf("readiness probe cannot be nil")
	}
	if len(rp.LogPatterns.Success) == 0 && len(rp.LogPatterns.Failure) == 0 && rp.Serve == nil {
		return fmt.Errorf("readiness probe must have at least one pattern or a serve check")
	}
	if rp.Serve != nil && rp.Serve.Path == "" {
		return fmt.Errorf("readiness serve check must have a path")
	}
	if rp.Timeout <= 0 {
		return fmt.Errorf("readiness probe timeout must be positive")
	}
	for i := range rp.LogPatterns.Success {
		if err := rp.LogPatterns.Success[i].Compile(); err != nil {
			return fmt.Errorf("readiness success pattern %d: %w", i, err)
		}
	}
	for i := range rp.LogPatterns.Failure {
		if err := rp.LogPatterns.Failure[i].Compile(); err != nil {
			return fmt.Errorf("readiness failure pattern %d: %w", i, err)
		}
	}
	return nil
}

// LivenessProbe defines how to detect if a running process has failed.
type LivenessProbe struct {
	LogPatterns         LogPatterns `yaml:"logPatterns" json:"log_patterns"`
	InitialDelaySeconds int         `yaml:"initialDelaySeconds" json:"initial_delay_seconds"`
}

// Validate checks probe configuration.
func (lp *LivenessProbe) Validate() error {
	if lp == nil {
		return fmt.Errorf("liveness probe cannot be nil")
	}
	if len(lp.LogPatterns.Failure) == 0 {
		return fmt.Errorf("liveness probe must have at least one failure pattern")
	}
	if lp.InitialDelaySeconds < 0 {
		return fmt.Errorf("initialDelaySeconds must be non-negative")
	}
	for i := range lp.LogPatterns.Failure {
		if err := lp.LogPatterns.Failure[i].Compile(); err != nil {
			return fmt.Errorf("liveness failure pattern %d: %w", i, err)
		}
	}
	return nil
}

// LogPatterns defines success and failure patterns for log-based health detection.
type LogPatterns struct {
	Success []PatternMatcher `yaml:"success,omitempty" json:"success,omitempty"`
	Failure []PatternMatcher `yaml:"failure,omitempty" json:"failure,omitempty"`
}

// MatchSuccess checks if a line matches any success pattern.
func (lp *LogPatterns) MatchSuccess(line string) (bool, string) {
	for _, p := range lp.Success {
		if p.Match(line) {
			return true, p.Pattern
		}
	}
	return false, ""
}

// MatchFailure checks if a line matches any failure pattern.
func (lp *LogPatterns) MatchFailure(line string) (bool, string) {
	for _, p := range lp.Failure {
		if p.Match(line) {
			return true, p.Pattern
		}
	}
	return false, ""
}

// PatternMatcher matches a string or regex pattern against log lines.
type PatternMatcher struct {
	Pattern       string         `yaml:"pattern" json:"pattern"`
	IsRegex       bool           `yaml:"isRegex,omitempty" json:"is_regex,omitempty"`
	compiledRegex *regexp.Regexp `yaml:"-" json:"-"`
}

// Compile compiles the pattern if it's a regex.
func (pm *PatternMatcher) Compile() error {
	if !pm.IsRegex || pm.Pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pm.Pattern)
	if err != nil {
		return fmt.Errorf("invalid regex %q: %w", pm.Pattern, err)
	}
	pm.compiledRegex = re
	return nil
}

// Match checks if a line matches this pattern.
func (pm *PatternMatcher) Match(line string) bool {
	if pm.Pattern == "" {
		return false
	}
	if pm.IsRegex {
		if pm.compiledRegex == nil {
			if err := pm.Compile(); err != nil {
				return false
			}
		}
		return pm.compiledRegex.MatchString(line)
	}
	return strings.Contains(line, pm.Pattern)
}

// CheckConfig holds health check configuration for an instance.
type CheckConfig struct {
	// Legacy log patterns
	LogReadyPatterns []string

	// HTTP health check
	HTTPHealthPath   string
	HTTPHealthMethod string
	ExpectedStatus   int
	StartupTimeout   time.Duration

	// Timing overrides (0 = use global default)
	Interval   time.Duration
	Timeout    time.Duration
	MaxRetries int

	// Kubernetes-style probes
	ReadinessProbe *ReadinessProbe
	LivenessProbe  *LivenessProbe
}

// ServeCheck returns the declared serve check, or nil when the provider
// relies on the plain health endpoint.
func (c CheckConfig) ServeCheck() *ServeCheck {
	if c.ReadinessProbe == nil {
		return nil
	}
	return c.ReadinessProbe.Serve
}
