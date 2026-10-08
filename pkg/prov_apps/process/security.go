package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/stperic/zzrouter/pkg/host"
)

// dangerousPaths lists filesystem paths that should never be targets of symlinks,
// file mounts, or other user-supplied path operations.
var dangerousPaths = []string{"/etc", "/root", "/sys", "/proc", "/boot", "/dev"}

// modelNameRegex validates model names before they reach command construction.
// Rejects shell metacharacters, newlines, and path traversal sequences.
var modelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9._:/@+-]+$`)

// ValidateModelName checks that a model name contains only safe characters.
func ValidateModelName(model string) error {
	if model == "" {
		return nil // empty model is valid (some ops don't need one)
	}
	if !modelNameRegex.MatchString(model) {
		return fmt.Errorf("invalid model name %q: must match %s", model, modelNameRegex.String())
	}
	if strings.Contains(model, "..") {
		return fmt.Errorf("invalid model name %q: path traversal not allowed", model)
	}
	return nil
}

// SecurityConfig defines security validation rules for process execution.
type SecurityConfig struct {
	AllowedImagePatterns   []string `json:"allowed_image_patterns"`
	AllowedCommands        []string `json:"allowed_commands"`
	AllowedVolumePaths     []string `json:"allowed_volume_paths"`
	AllowedExecutablePaths []string `json:"allowed_executable_paths"`
	ValidateExecutables    bool     `json:"validate_executables"`
	EnableSandboxing       bool     `json:"enable_sandboxing"`
}

// DefaultSecurityConfig returns secure defaults for production use.
func DefaultSecurityConfig() *SecurityConfig {
	return &SecurityConfig{
		AllowedImagePatterns: []string{
			"vllm/vllm*",
			"ollama/ollama*",
			"ghcr.io/ggerganov/llama.cpp*",
		},
		AllowedCommands: []string{
			"python",
			"serve",
			"llama-server",
		},
		AllowedVolumePaths: []string{
			"/tmp/*",
			"/var/tmp/*",
			"~/.cache/*",
			"~/.local/*",
			"~/Library/Application Support/zzrouter/*",
			"~/.config/zzrouter/*",
		},
		AllowedExecutablePaths: []string{
			"/usr/local/bin/*",
			"/opt/*/bin/*",
			"/opt/*",
			"/opt/homebrew/bin/*",
			"~/.local/bin/*",
		},
		ValidateExecutables: true,
		EnableSandboxing:    true,
	}
}

// SecurityValidator validates process configurations.
//
// Config-driven apps (from provider config) are TRUSTED — skip command validation.
// Ad-hoc commands (from API/CLI) are UNTRUSTED — require strict validation.
type SecurityValidator struct {
	config *SecurityConfig
}

// NewSecurityValidator creates a validator with the given config.
// Uses defaults if config is nil.
func NewSecurityValidator(config *SecurityConfig) *SecurityValidator {
	if config == nil {
		config = DefaultSecurityConfig()
	}
	return &SecurityValidator{config: config}
}

// ValidateCommand validates a command against the whitelist.
// Only call this for ad-hoc commands, not config-driven apps.
func (sv *SecurityValidator) ValidateCommand(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("empty command not allowed")
	}

	baseCmd := filepath.Base(command[0])

	for _, allowed := range sv.config.AllowedCommands {
		//nolint:gosec // guarded by len(command)==0 check above
		if baseCmd == allowed || command[0] == allowed {
			return sv.validateCommandArgs(command[1:]) //nolint:gosec // len>=1 guaranteed
		}
	}

	return fmt.Errorf("command %q not in allowed commands: %v", baseCmd, sv.config.AllowedCommands)
}

// validateCommandArgs checks for dangerous shell metacharacters and patterns.
func (sv *SecurityValidator) validateCommandArgs(args []string) error {
	for _, arg := range args {
		// Shell metacharacters + newlines (injection vectors)
		if strings.ContainsAny(arg, ";|&$`<>(){}[]\n\r") {
			return fmt.Errorf("dangerous character in argument: %s", arg)
		}
	}

	dangerousPatterns := []string{
		`rm\s*-rf`, `rm\s*-fr`, `sudo`, `su\s`, `chmod\s*777`,
		`wget\s+http`, `curl\s+http`, `eval`, `exec`,
	}

	argsStr := strings.Join(args, " ")
	for _, pattern := range dangerousPatterns {
		matched, _ := regexp.MatchString(pattern, argsStr)
		if matched {
			return fmt.Errorf("dangerous command pattern: %s", pattern)
		}
	}

	return nil
}

// ValidateVolumeMount validates a volume mount path against the allowlist.
func (sv *SecurityValidator) ValidateVolumeMount(hostPath string) error {
	if hostPath == "" {
		return fmt.Errorf("empty volume path not allowed")
	}

	expandedPath := expandHomePath(hostPath)

	for _, allowedPath := range sv.config.AllowedVolumePaths {
		expandedAllowed := expandHomePath(allowedPath)

		if before, ok := strings.CutSuffix(expandedAllowed, "*"); ok {
			if strings.HasPrefix(expandedPath, before) {
				return nil
			}
		} else if expandedPath == expandedAllowed {
			return nil
		}
	}

	return fmt.Errorf("volume path %q not in allowed paths", hostPath)
}

// ValidateExecutable validates an executable path for security.
func (sv *SecurityValidator) ValidateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("executable not found: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory, not an executable")
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("file is not executable")
	}

	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	if err := checkSymlinkSafety(realPath); err != nil {
		return err
	}

	return sv.checkAllowedPath(realPath)
}

// checkSymlinkSafety verifies a resolved path doesn't point to dangerous system locations.
func checkSymlinkSafety(realPath string) error {
	for _, dangerous := range dangerousPaths {
		if strings.HasPrefix(realPath, dangerous+"/") || realPath == dangerous {
			return fmt.Errorf("symlink to dangerous location: %s", dangerous)
		}
	}
	return nil
}

// checkAllowedPath verifies the executable is in an allowed path.
func (sv *SecurityValidator) checkAllowedPath(realPath string) error {
	if !sv.config.ValidateExecutables || len(sv.config.AllowedExecutablePaths) == 0 {
		return nil
	}

	for _, allowedPath := range sv.config.AllowedExecutablePaths {
		expanded := expandHomePath(allowedPath)
		if matched, _ := filepath.Match(expanded, realPath); matched {
			return nil
		}
	}
	return fmt.Errorf("executable not in allowed paths")
}

// ValidateExecutableVersion runs --version and returns parsed output.
func (sv *SecurityValidator) ValidateExecutableVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := host.CommandContext(ctx, path, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("version check failed: %w (output: %s)", err, string(output))
	}

	return ParseVersionFromOutput(string(output)), nil
}

// SandboxCommand wraps a command with sandboxing (nice for lower priority).
func (sv *SecurityValidator) SandboxCommand(command []string) []string {
	if !sv.config.EnableSandboxing {
		return command
	}

	// `nice` has no native Windows equivalent. Some Git-for-Windows installs
	// put a MinGW nice.exe on PATH, but that tool does not interoperate with
	// Windows process semantics -- spawning `nice -n 10 foo.exe` via zzrouter-
	// launcher errors out with exit code 2816 before the child even starts.
	// Windows has SetPriorityClass (Win32 API) and START /LOW; if we want
	// CPU-throttling for child providers later, wire it through those. For
	// now the sandbox is a no-op on Windows.
	if runtime.GOOS == "windows" {
		return command
	}

	if _, err := exec.LookPath("nice"); err == nil {
		return append([]string{"nice", "-n", "10"}, command...)
	}

	return command
}

// paramKeyRegex validates parameter keys — alphanumeric, hyphens, underscores only.
var paramKeyRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// dangerousEnvVars is a blocklist of environment variable names that could enable
// code injection or privilege escalation when set on child processes.
var dangerousEnvVars = map[string]bool{
	// Dynamic linker injection (Unix)
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
	"LD_AUDIT":        true,
	// Dynamic linker injection (macOS)
	"DYLD_INSERT_LIBRARIES":      true,
	"DYLD_LIBRARY_PATH":          true,
	"DYLD_FRAMEWORK_PATH":        true,
	"DYLD_FALLBACK_LIBRARY_PATH": true,
	// Language runtime injection
	"PYTHONPATH":    true,
	"PYTHONSTARTUP": true,
	"PYTHONHOME":    true,
	"NODE_OPTIONS":  true,
	"RUBYOPT":       true,
	// Path hijacking
	"PATH": true,
}

// dangerousEnvPrefixes are prefixes that indicate env vars carrying zzRouter secrets.
var dangerousEnvPrefixes = []string{
	"ZZROUTER_",
}

// IsDangerousEnvVar returns true if the environment variable key could enable
// code injection, dynamic linker attacks, or leak zzRouter secrets.
func IsDangerousEnvVar(key string) bool {
	upper := strings.ToUpper(key)
	if dangerousEnvVars[upper] {
		return true
	}
	for _, prefix := range dangerousEnvPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// maxParamValueLen is the maximum length for any single parameter value.
// Prevents unbounded allocation in CLI arg construction and provider binaries
// that parse argv into fixed buffers.
const maxParamValueLen = 4096

// paramValueRegex restricts parameter values to a safe character set.
// Designed to allow legitimate LLM provider flags while blocking shell
// metacharacters, command substitution, and other injection vectors.
//
// Allowed: alphanumeric, dots, underscores, hyphens, colons, path separators,
// at-signs, plus, equals, spaces, commas, percent, tilde.
// This covers: version strings ("0.95"), negative numbers ("-1"),
// paths ("/models/llama"), pip specifiers ("pkg@v1"), URLs, etc.
//
// Blocked: semicolons, pipes, backticks, dollar signs, parentheses,
// angle brackets, ampersands, exclamation marks, quotes, braces,
// brackets, hash, question marks, asterisks.
// Any of these in a provider CLI argument is almost certainly an
// injection attempt or a misconfiguration.
var paramValueRegex = regexp.MustCompile(`^[a-zA-Z0-9._:/\\@+\-= ,~%\t]+$`)

// pathTraversalPattern detects directory traversal sequences.
// Even though we use exec.Command (no shell), a `--model ../../etc/passwd`
// could cause the provider binary to read arbitrary files.
var pathTraversalPattern = regexp.MustCompile(`(?:^(?:[a-zA-Z]:)?|[/\\])\.\.(?:[/\\]|$)`)

// ValidateParameters checks parameter keys and values for safety.
// Called unconditionally — even for trusted (config-driven) commands.
//
// Keys: alphanumeric + hyphens + underscores.
// Values: safe character set, max length, no path traversal, no shell
// metacharacters. Empty values are allowed (some flags are boolean).
func ValidateParameters(params map[string]string) error {
	for key, value := range params {
		if !paramKeyRegex.MatchString(key) {
			return fmt.Errorf("invalid parameter key %q: must contain only letters, digits, hyphens, underscores", key)
		}
		if err := validateParamValue(key, value); err != nil {
			return err
		}
	}
	return nil
}

// validateParamValue checks a single parameter value for safety.
func validateParamValue(key, value string) error {
	if value == "" {
		return nil
	}

	// Length limit
	if len(value) > maxParamValueLen {
		return fmt.Errorf("parameter %q value too long (%d chars, max %d)", key, len(value), maxParamValueLen)
	}

	// Null bytes and control characters (except tab)
	for i, r := range value {
		if r == 0 {
			return fmt.Errorf("parameter %q contains null byte at position %d", key, i)
		}
		if unicode.IsControl(r) && r != '\t' {
			return fmt.Errorf("parameter %q contains control character at position %d", key, i)
		}
	}

	// Safe character set — blocks shell metacharacters even though we use
	// exec.Command (defense in depth against future refactors or provider
	// binaries that re-parse their args through a shell).
	if !paramValueRegex.MatchString(value) {
		return fmt.Errorf("parameter %q value contains unsafe characters: only alphanumeric, dots, hyphens, underscores, colons, slashes, spaces, and common punctuation are allowed", key)
	}

	// Path traversal prevention
	if pathTraversalPattern.MatchString(value) {
		return fmt.Errorf("parameter %q value contains path traversal sequence (..)", key)
	}

	return nil
}

// childEnvAllowlist defines environment variable prefixes that are safe to
// inherit from the parent process into child LLM processes.
var childEnvAllowlist = []string{
	// POSIX essentials
	"HOME=", "USER=", "LOGNAME=", "LANG=", "LC_", "TERM=", "TMPDIR=", "TMP=", "TEMP=",
	"PATH=", "SHELL=",
	"XDG_",
	// Windows essentials: kernel/loader, user profile, and common runtime paths.
	// Without SYSTEMROOT/WINDIR, child processes can't resolve kernel32 and
	// similar DLLs -- spawning them yields STATUS_ACCESS_VIOLATION (0xC0000005)
	// on startup. USERPROFILE/APPDATA/LOCALAPPDATA/PROGRAMFILES are needed by
	// many third-party Windows apps (including llama.cpp) that call Win32 APIs
	// like SHGetKnownFolderPath to locate user data.
	"SYSTEMROOT=", "WINDIR=", "SYSTEMDRIVE=", "COMSPEC=",
	"USERPROFILE=", "APPDATA=", "LOCALAPPDATA=", "HOMEDRIVE=", "HOMEPATH=",
	"PROGRAMFILES=", "PROGRAMFILES(X86)=", "PROGRAMDATA=", "PROGRAMW6432=",
	"COMMONPROGRAMFILES=", "COMMONPROGRAMFILES(X86)=", "COMMONPROGRAMW6432=",
	"COMPUTERNAME=", "USERDOMAIN=", "USERNAME=",
	"NUMBER_OF_PROCESSORS=", "PROCESSOR_ARCHITECTURE=", "PROCESSOR_IDENTIFIER=",
	"PROCESSOR_LEVEL=", "PROCESSOR_REVISION=", "OS=",
	"PATHEXT=", "PSModulePath=",
	// ML/LLM runtime allowlist (same as before)
	"CUDA_", "NVIDIA_", "HIP_", "ROCR_", "GPU_",
	"MLX_", "VLLM_", "HF_", "HUGGING_FACE_", "TRANSFORMERS_",
	"OLLAMA_", "HTTP_PROXY=", "HTTPS_PROXY=", "NO_PROXY=",
	"http_proxy=", "https_proxy=", "no_proxy=",
}

// ChildEnvironment validates overrides and filters inherited server secrets.
func ChildEnvironment(userEnvVars map[string]string) ([]string, error) {
	for key := range userEnvVars {
		if IsDangerousEnvVar(key) {
			return nil, fmt.Errorf("%w: %s", ErrDangerousEnvVar, key)
		}
	}
	return buildChildEnvironment(userEnvVars), nil
}

// buildChildEnvironment creates a minimal environment for child processes
// by filtering the parent environment through an allowlist, then merging
// user-supplied env vars. This prevents leaking server secrets like
// ZZROUTER_ADMIN_API_KEY into child processes.
func buildChildEnvironment(userEnvVars map[string]string) []string {
	parentEnv := os.Environ()
	env := make([]string, 0, len(parentEnv)/2+len(userEnvVars)+1)

	// Filter parent environment through allowlist
	for _, e := range parentEnv {
		for _, prefix := range childEnvAllowlist {
			if strings.HasPrefix(e, prefix) {
				env = append(env, e)
				break
			}
		}
	}

	// Add user-supplied env vars (already validated by IsDangerousEnvVar)
	for key, value := range userEnvVars {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	// Add process marker for orphan detection
	env = append(env, fmt.Sprintf("%s=%s", ManagedProcessEnvKey, ManagedProcessEnvValue))

	return env
}

// Compiled version-extraction patterns (hoisted to avoid per-call re-compilation).
var versionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`v?(\d+\.\d+\.\d+[-\w]*)`),
	regexp.MustCompile(`version\s+v?(\d+\.\d+\.\d+[-\w]*)`),
	regexp.MustCompile(`(\d+\.\d+\.\d+)`),
}

// ParseVersionFromOutput extracts a version string from command output.
func ParseVersionFromOutput(output string) string {
	for _, re := range versionPatterns {
		if matches := re.FindStringSubmatch(output); len(matches) > 1 {
			return matches[1]
		}
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 0 && len(lines[0]) > 0 {
		if len(lines[0]) > 50 {
			return lines[0][:50] + "..."
		}
		return lines[0]
	}

	return "unknown"
}

func expandHomePath(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(homeDir, path[2:])
}
