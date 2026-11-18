package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/zzrouter/pkg/security"
)

// InstallationCredentialOptions describes a legacy file whose provenance the
// service manager has established. Arbitrary environment overrides are not saved.
type InstallationCredentialOptions struct {
	LegacyFile string
	// LegacyOverrides is true only when the installed service imports LegacyFile.
	LegacyOverrides bool
}

// EnsureInstallationCredentials creates or preserves the canonical installation
// credential file. It never prints a key, rotates an existing key, or edits YAML.
func EnsureInstallationCredentials(ctx context.Context, dir string, opts InstallationCredentialOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if dir == "" {
		return "", errors.New("installation config directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	// Mode bits do not protect Windows atomic-write temporary files. Protect
	// their containing directory before writing the first credential byte.
	if runtime.GOOS == "windows" {
		if err := security.RestrictDirectoryPermissions(dir); err != nil {
			return "", err
		}
	}
	lock, err := atomicfile.Lock(filepath.Join(dir, "credentials.lock"))
	if err != nil {
		return "", fmt.Errorf("lock installation credentials: %w", err)
	}
	defer func() { _ = lock.Close() }()
	path := filepath.Join(dir, ".env")
	content, values, err := readInstallationEnvironment(path)
	if err != nil {
		return "", err
	}
	updates := make(map[string]string)
	if opts.LegacyFile != "" && filepath.Clean(opts.LegacyFile) != filepath.Clean(path) {
		_, legacy, err := readInstallationEnvironment(opts.LegacyFile)
		if err != nil {
			return "", err
		}
		for key, value := range legacy {
			if value == "" {
				continue
			}
			current := values[key]
			if current == "" || opts.LegacyOverrides && installationAuthVariable(key) {
				values[key], updates[key] = value, value
			} else if current != value && !installationAuthVariable(key) {
				return "", fmt.Errorf("conflicting installation settings in %s and %s; reconcile the files before continuing", path, opts.LegacyFile)
			}
		}
	}
	const adminVariable = "ZZROUTER_ADMIN_API_KEY"
	if values[adminVariable] == "" {
		key, err := GenerateAPIKey()
		if err != nil {
			return "", err
		}
		values[adminVariable], updates[adminVariable] = key, key
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(updates) > 0 {
		if err := atomicfile.WriteFile(path, updateInstallationEnvironment(content, updates), 0o600); err != nil {
			return "", fmt.Errorf("save installation credentials: %w", err)
		}
	}
	if err := security.RestrictFilePermissions(path); err != nil {
		return "", fmt.Errorf("protect installation credentials: %w", err)
	}
	return values[adminVariable], nil
}

func installationAuthVariable(key string) bool {
	return key == "ZZROUTER_ADMIN_API_KEY" || key == "ZZROUTER_API_KEY" || key == "ZZROUTER_CLUSTER_NETWORK_KEY"
}

func readInstallationEnvironment(path string) ([]byte, map[string]string, error) {
	values := make(map[string]string)
	stat, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, values, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect installation environment %s: %w", path, err)
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return nil, nil, fmt.Errorf("installation environment %s must be a regular file smaller than 1 MiB", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read installation environment %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !isEnvVarName(key) {
			return nil, nil, fmt.Errorf("invalid assignment in installation environment %s", path)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate assignment in installation environment %s", path)
		}
		values[key] = unquoteValue(strings.TrimSpace(value))
	}
	return data, values, nil
}

func updateInstallationEnvironment(content []byte, updates map[string]string) []byte {
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	remaining := make(map[string]string, len(updates))
	for key, value := range updates {
		remaining[key] = value
	}
	for i, line := range lines {
		key, _, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		key = strings.TrimSpace(key)
		if value, replace := remaining[key]; ok && replace {
			lines[i] = key + "=" + value
			delete(remaining, key)
		}
	}
	keys := make([]string, 0, len(remaining))
	for key := range remaining {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, key+"="+remaining[key])
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
