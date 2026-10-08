// Package config provides OS-agnostic configuration file management for zzRouter
// .env file loading utilities
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func getConfigDir() string {
	return Paths().GetConfigDir()
}

// LoadDotEnvFrom loads environment variables from a file.
// Existing env vars take precedence (won't overwrite).
func LoadDotEnvFrom(filename string) (int, error) {
	return loadDotEnvFile(filename, false)
}

// ReloadDotEnvFrom loads environment variables from a file, overwriting existing values.
// Used to pick up changes made by another process (e.g., TUI saved a new API key).
func ReloadDotEnvFrom(filename string) (int, error) {
	return loadDotEnvFile(filename, true)
}

// loadDotEnvFile is the shared implementation for loading .env files.
// When overwrite is false, existing env vars take precedence.
// When overwrite is true, file values always win (for reload scenarios).
func loadDotEnvFile(filename string, overwrite bool) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to open %s: %w", filename, err)
	}
	defer func() { _ = file.Close() }()

	// Reject world-readable .env files (security risk — may contain API keys).
	if isWorldReadablePath(filename) {
		return 0, fmt.Errorf("SECURITY: %s is world-readable. "+
			"Fix with: chmod 600 %s (Linux/macOS) or tighten NTFS ACL (Windows)", filename, filename)
	}

	count := 0
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		line = strings.TrimPrefix(line, "export ")

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, " WARNING: Invalid line %d in %s: %s\n", lineNum, filename, line)
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := unquoteValue(strings.TrimSpace(parts[1]))

		if overwrite || os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil { // lint:allow os.Setenv
				fmt.Fprintf(os.Stderr, " WARNING: Failed to set %s: %v\n", key, err)
				continue
			}
			count++
		}
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("error reading %s: %w", filename, err)
	}

	return count, nil
}

// unquoteValue removes surrounding quotes from a value
func unquoteValue(value string) string {
	// Remove single quotes
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}

	// Remove double quotes
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}

	return value
}

// LoadDotEnvWithPriority loads .env files in priority order:
// 1. .env.local (highest priority, git-ignored, local overrides)
// 2. .env.{environment} (e.g., .env.production, .env.development)
// 3. .env (default, usually committed)
//
// This follows common practices from Node.js and other ecosystems
func LoadDotEnvWithPriority(environment string) (int, error) {
	// Validate environment name to prevent path traversal
	if err := validateEnvironmentName(environment); err != nil {
		return 0, err
	}

	totalCount := 0

	// Priority 3: .env (default)
	if count, err := LoadDotEnvFrom(".env"); err != nil {
		return totalCount, err
	} else {
		totalCount += count
	}

	// Priority 2: .env.{environment}
	if environment != "" {
		envFile := fmt.Sprintf(".env.%s", environment)
		if count, err := LoadDotEnvFrom(envFile); err != nil {
			return totalCount, err
		} else {
			totalCount += count
		}
	}

	// Priority 1: .env.local (highest priority, overrides everything)
	if count, err := LoadDotEnvFrom(".env.local"); err != nil {
		return totalCount, err
	} else {
		totalCount += count
	}

	return totalCount, nil
}

// GetDotEnvLocations returns the paths where .env files are checked
// Returns: [current_working_directory, config_directory]
func GetDotEnvLocations() []string {
	locations := []string{}

	// Location 1: Current working directory
	if cwd, err := os.Getwd(); err == nil {
		locations = append(locations, filepath.Join(cwd, ".env"))
	}

	// Location 2: Config directory
	configDir := getConfigDir()
	if configDir != "" {
		locations = append(locations, filepath.Join(configDir, ".env"))
	}

	return locations
}

// AutoLoadDotEnv automatically loads .env files with smart defaults
// Checks two locations:
// 1. Current working directory (./.env)
// 2. Config directory (~/Library/Application Support/zzrouter/.env)
func AutoLoadDotEnv() error {
	// Get environment (defaults to "development")
	environment := os.Getenv("ZZROUTER_ENV")
	if environment == "" {
		environment = os.Getenv("NODE_ENV") // Common convention
	}
	if environment == "" {
		environment = "development"
	}

	totalCount := 0

	// Load from current working directory (skip in production)
	if environment != "production" {
		if count, err := LoadDotEnvWithPriority(environment); err != nil {
			return err
		} else {
			totalCount += count
		}
	}

	// Always load from config directory — this is where API keys live.
	// The file has 0600 permissions and is the canonical location for secrets.
	configDir := getConfigDir()
	if configDir != "" {
		if count, err := loadDotEnvWithPriorityFromDir(environment, configDir); err == nil {
			totalCount += count
		}
	}

	if totalCount > 0 {
		fmt.Printf("Loaded %d environment variable(s) from .env files\n", totalCount)
	}

	return nil
}

// loadDotEnvWithPriorityFromDir loads .env files from a specific directory using absolute paths.
// This avoids the os.Chdir race condition that affects concurrent goroutines.
func loadDotEnvWithPriorityFromDir(environment, dir string) (int, error) {
	// Validate environment name to prevent path traversal
	if err := validateEnvironmentName(environment); err != nil {
		return 0, err
	}

	totalCount := 0

	// Priority 3: .env (default)
	if count, err := LoadDotEnvFrom(filepath.Join(dir, ".env")); err != nil {
		return totalCount, err
	} else {
		totalCount += count
	}

	// Priority 2: .env.{environment}
	if environment != "" {
		envFile := filepath.Join(dir, fmt.Sprintf(".env.%s", environment))
		if count, err := LoadDotEnvFrom(envFile); err != nil {
			return totalCount, err
		} else {
			totalCount += count
		}
	}

	// Priority 1: .env.local (highest priority)
	if count, err := LoadDotEnvFrom(filepath.Join(dir, ".env.local")); err != nil {
		return totalCount, err
	} else {
		totalCount += count
	}

	return totalCount, nil
}

// validEnvNamePattern matches only alphanumeric, dash, underscore, and dot characters
var validEnvNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validateEnvironmentName rejects environment names that could cause path traversal
func validateEnvironmentName(environment string) error {
	if environment == "" {
		return nil
	}
	if strings.Contains(environment, "/") || strings.Contains(environment, "\\") ||
		strings.Contains(environment, "..") || strings.Contains(environment, "\x00") {
		return fmt.Errorf("invalid environment name %q: contains path traversal characters", environment)
	}
	if !validEnvNamePattern.MatchString(environment) {
		return fmt.Errorf("invalid environment name %q: must contain only alphanumeric, dash, underscore, or dot characters", environment)
	}
	return nil
}

// ShouldLoadDotEnv determines if .env files should be loaded.
// Always returns true unless explicitly disabled — the config dir .env
// is the canonical location for API keys and must always be loaded.
func ShouldLoadDotEnv() bool {
	return os.Getenv("ZZROUTER_DISABLE_DOTENV") != "true"
}

// LoadConfigDirEnv loads the .env file from the config directory.
// Only sets vars that aren't already set (safe for startup).
// Shared by both LoadClientConfig and LoadNodeConfig to resolve env var references.
func (cm *ConfigManager) LoadConfigDirEnv() {
	envFile := filepath.Join(cm.GetNodeConfigDir(), ".env")
	_, _ = LoadDotEnvFrom(envFile)
}

// ReloadConfigDirEnv forces a reload of the .env file, overwriting existing values.
// Used after a key is saved to pick up changes in a running process.
func (cm *ConfigManager) ReloadConfigDirEnv() {
	envFile := filepath.Join(cm.GetNodeConfigDir(), ".env")
	_, _ = ReloadDotEnvFrom(envFile)
}

// GetEnvFilePath returns the path to the .env file in the config directory.
func GetEnvFilePath() string {
	return filepath.Join(getConfigDir(), ".env")
}

// SetEnvVar writes or updates a KEY=value pair in the .env file in the config directory.
// Creates the file if it doesn't exist. Uses 0600 permissions for security.
// Also calls os.Setenv so the value takes effect immediately in the current process.
func SetEnvVar(key, value string) error {
	envPath := GetEnvFilePath()

	// Read existing content
	existing := make(map[string]string)
	var orderedKeys []string
	if data, err := os.ReadFile(envPath); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if idx := strings.IndexByte(line, '='); idx > 0 {
				k := strings.TrimSpace(line[:idx])
				existing[k] = strings.TrimSpace(line[idx+1:])
				orderedKeys = append(orderedKeys, k)
			}
		}
	}

	// Update or add
	if _, exists := existing[key]; !exists {
		orderedKeys = append(orderedKeys, key)
	}
	existing[key] = value

	// Write back
	var lines []string
	for _, k := range orderedKeys {
		lines = append(lines, fmt.Sprintf("%s=%s", k, existing[k]))
	}
	content := strings.Join(lines, "\n") + "\n"

	if err := os.MkdirAll(filepath.Dir(envPath), 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}

	// Set immediately in current process
	return os.Setenv(key, value) // lint:allow os.Setenv
}

// RemoveEnvVar removes a KEY from the .env file and unsets it in the current process.
func RemoveEnvVar(key string) error {
	envPath := GetEnvFilePath()

	var lines []string
	if data, err := os.ReadFile(envPath); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if idx := strings.IndexByte(trimmed, '='); idx > 0 {
				if strings.TrimSpace(trimmed[:idx]) == key {
					continue // skip this key
				}
			}
			lines = append(lines, trimmed)
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("write .env: %w", err)
	}

	return os.Unsetenv(key)
}
