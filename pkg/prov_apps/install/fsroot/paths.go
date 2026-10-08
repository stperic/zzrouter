package fsroot

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/stperic/zzrouter/pkg/config"
)

// serviceUser is the dedicated Linux service user that owns provider directories.
const serviceUser = "zzrouter"

// providerRootOverride, when non-empty, wins over the platform default below.
// Set once at server boot from node.yaml `providers.install_dir` via
// SetProviderRootOverride. Read-mostly after startup; a plain string under
// a mutex would also work, but atomic.Pointer keeps the fast path lock-free.
var providerRootOverride atomic.Pointer[string]

// SetProviderRootOverride installs a global override for ProviderRootDir.
// An empty string clears the override. Callers set this at boot from
// node.yaml; tests can set and clear it freely.
func SetProviderRootOverride(path string) {
	if path == "" {
		providerRootOverride.Store(nil)
		return
	}
	p := path
	providerRootOverride.Store(&p)
}

// ProviderRootDir returns the parent directory for all provider installs.
// Precedence: node.yaml `providers.install_dir` override > platform default.
// Platform defaults:
//   - Linux:   /opt/zzrouter/providers/                          (machine-wide, chowned to service user)
//   - macOS:   ~/Library/Application Support/zzrouter/providers/ (per-user)
//   - Windows: %LOCALAPPDATA%\zzrouter\providers\                (per-user, non-roamed)
//
// Windows notes:
//   - %LOCALAPPDATA% (not %APPDATA%/Roaming) is chosen deliberately: provider
//     installs can be multi-GB (vllm wheels, llama.cpp binaries + weights),
//     and Roaming AppData is replicated by Windows roaming profiles in
//     domain environments. Local AppData stays on the machine.
//   - Using a per-user location avoids the admin requirement implied by
//     %ProgramFiles%, matching scripts/install.ps1's non-admin default
//     install path (%LOCALAPPDATA%\Programs\zzrouter).
func ProviderRootDir() string {
	if p := providerRootOverride.Load(); p != nil {
		return *p
	}
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "zzrouter", "providers")
	case "windows":
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			// Fallback: %USERPROFILE%\AppData\Local. Extremely rare to be empty.
			if home, err := os.UserHomeDir(); err == nil {
				localAppData = filepath.Join(home, "AppData", "Local")
			}
		}
		return filepath.Join(localAppData, "zzrouter", "providers")
	default: // linux
		return filepath.Join("/opt", "zzrouter", "providers")
	}
}

// EnsureProviderRoot creates the provider root directory with correct ownership.
// On Linux, if running as root and the zzrouter user exists, the directory is
// chowned to zzrouter:zzrouter so the service process can write to it.
// On macOS and Windows, it simply creates the directory (current user owns it).
func EnsureProviderRoot() error {
	root := ProviderRootDir()

	if err := os.MkdirAll(root, 0750); err != nil {
		return fmt.Errorf("failed to create provider root %s: %w", root, err)
	}

	// On Linux, chown to zzrouter user if running as root
	if runtime.GOOS == "linux" && os.Getuid() == 0 {
		u, err := user.Lookup(serviceUser)
		if err != nil {
			// zzrouter user doesn't exist yet — directory was created by root,
			// which is fine; SetupDirectories will fix ownership later.
			return nil //nolint:nilerr // missing service user is expected on first-run
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.Chown(root, uid, gid); err != nil {
			return fmt.Errorf("failed to chown %s to %s: %w", root, serviceUser, err)
		}
	}

	return nil
}

// ProviderDir returns the install directory for a provider.
// Delegates to ProviderRootDir so the Windows/macOS/Linux conventions
// documented there stay in one place.
func ProviderDir(name string) string {
	return filepath.Join(ProviderRootDir(), name)
}

// ProviderBinDir returns the bin directory inside a provider's install dir.
func ProviderBinDir(name string) string {
	return filepath.Join(ProviderDir(name), "bin")
}

// ProviderBinStagingDir returns the directory an install unpacks into
// before it replaces the live bin/. It is a sibling of ProviderBinDir so
// that activating the new tree is a rename within one filesystem rather
// than a copy.
func ProviderBinStagingDir(name string) string {
	return filepath.Join(ProviderDir(name), "bin.incoming")
}

// ProviderBinPreviousDir returns where the live bin/ is parked while a
// staged tree takes its place. It exists only for the duration of the
// activation step: anything left behind there is the debris of a failed
// activation, not a backup anyone should restore from.
func ProviderBinPreviousDir(name string) string {
	return filepath.Join(ProviderDir(name), "bin.previous")
}

// ProviderVenvDir returns the Python venv directory for a provider.
func ProviderVenvDir(name string) string {
	return filepath.Join(ProviderDir(name), "venv")
}

// ProviderVenvPython returns the Python executable inside a provider's venv.
func ProviderVenvPython(name string) string {
	venv := ProviderVenvDir(name)
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python3")
}

// ProviderVenvPip returns the pip executable inside a provider's venv.
func ProviderVenvPip(name string) string {
	venv := ProviderVenvDir(name)
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "pip.exe")
	}
	return filepath.Join(venv, "bin", "pip")
}

// ProviderVersionFile returns the path to the version file for a provider.
// Written at install time, read at startup — avoids shelling out to detect versions.
func ProviderVersionFile(name string) string {
	return filepath.Join(ProviderDir(name), "version")
}

// ReadInstalledVersion reads the installed version from the version file.
// Returns empty string if the file doesn't exist or can't be read.
func ReadInstalledVersion(name string) string {
	data, err := os.ReadFile(ProviderVersionFile(name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteVersionCommand returns a platform-appropriate shell command to write a version file.
func WriteVersionCommand(version, path string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("powershell -NoProfile -Command \"Set-Content -Path %s -Value '%s'\"",
			PowerShellQuote(path), version)
	}
	return fmt.Sprintf("printf '%%s\\n' %s > %s", ShellQuote(version), ShellQuote(path))
}

// ProviderBackupDir returns the backup directory for rollback support.
// This is a sibling of the install directory (not a subdirectory) to prevent
// rm -rf on the install dir from destroying the backup.
func ProviderBackupDir(name string) string {
	return ProviderDir(name) + ".backup"
}

// ManagedMarkerPath returns the path to the .managed marker file for a provider.
// This file is written at install time by zzRouter to distinguish "we installed it"
// from "user installed it independently". Used by IsInstalled() to gate uninstall.
func ManagedMarkerPath(providerName string) string {
	return filepath.Join(ProviderDir(providerName), ".managed")
}

// ProviderManifestPath returns the path to the JSON install manifest for a
// provider. Written at install time alongside the version file; captures
// invariants that survive the installer dispatcher exiting (chosen
// interpreter, resolved realpath fingerprint, etc.) so a later process
// can verify drift without re-running the whole selection.
func ProviderManifestPath(name string) string {
	return filepath.Join(ProviderDir(name), "install.json")
}

// InstallManifest records the decisions made at install time that we need
// to verify again at subsequent starts. Today it covers Python providers;
// non-Python providers write an empty Interpreter section and the integrity
// check skips it.
//
// Why this file exists: `python3 -m venv` in the install step creates a
// venv whose internal `bin/python3` symlinks to a specific Python framework
// path on the host. When Homebrew point-upgrades `python@3.13` from 3.13.4
// → 3.13.5 it can garbage-collect the previous Cellar tree, leaving the
// venv's internal symlinks dangling. The live failure mode is silent — the
// provider binary "exists" but execing it fails with "dyld: could not load
// library". Recording the resolved realpath here lets HealthCheck diff it
// at startup and raise a clean "reinstall needed" signal instead of
// surfacing an incomprehensible dyld error from first-chat.
type InstallManifest struct {
	PlanID            string                `json:"plan_id,omitempty"`
	Inventory         []ManifestPackage     `json:"dependency_inventory,omitempty"`
	RecipeFingerprint string                `json:"recipe_fingerprint,omitempty"`
	PolicyFingerprint string                `json:"policy_fingerprint,omitempty"`
	Recipe            *config.InstallRecipe `json:"recipe,omitempty"`
	Version           string                `json:"version"`                // Provider version ("0.31.2", "b5604")
	InstalledAt       string                `json:"installed_at,omitempty"` // RFC3339 timestamp
	Interpreter       *ManifestInterpreter  `json:"interpreter,omitempty"`  // nil for non-Python providers
}

// ManifestPackage records the accepted resolver provenance of an installed distribution.
type ManifestPackage struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Requested   bool   `json:"requested"`
	ArtifactURL string `json:"artifact_url"`
	SHA256      string `json:"sha256"`
}

// ManifestInterpreter captures the Python interpreter selected at install
// time. Path is the stable alias (e.g., /opt/homebrew/opt/python@3.13/bin/
// python3.13) and survives point-upgrades; Realpath is the dereferenced
// absolute path at selection time and is compared against re-resolution
// for drift detection. Version lets HealthCheck verify the alias still
// resolves to a compatible minor even if Realpath has shifted.
type ManifestInterpreter struct {
	Path     string `json:"path"`
	Realpath string `json:"realpath,omitempty"`
	Version  string `json:"version"`
}

// ReadInstallManifest loads the manifest for a provider, returning
// (nil, nil) when the file doesn't exist. A malformed manifest returns
// an error rather than being silently treated as absent — a truncated
// manifest is a different failure class than a clean-slate install and
// callers should not conflate them.
func ReadInstallManifest(name string) (*InstallManifest, error) {
	data, err := os.ReadFile(ProviderManifestPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m InstallManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("corrupt manifest at %s: %w", ProviderManifestPath(name), err)
	}
	return &m, nil
}

// WriteInstallManifest writes a manifest atomically (write-temp + rename)
// so an interrupted install never leaves the manifest half-parseable.
// Directory perms are 0750 to match the broader provider-root convention.
func WriteInstallManifest(name string, m *InstallManifest) error {
	path := ProviderManifestPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return fmt.Errorf("manifest parent dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("manifest encode: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0640); err != nil {
		return fmt.Errorf("manifest write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("manifest rename: %w", err)
	}
	return nil
}

// ManagedMarkerExists returns true if the provider was installed by zzRouter.
func ManagedMarkerExists(providerName string) bool {
	_, err := os.Stat(ManagedMarkerPath(providerName))
	return err == nil
}

// ShellQuote wraps a path for safe use in shell command strings.
// On Unix: single-quotes the path (escaping embedded single quotes).
// On Windows: double-quotes the path (for cmd.exe).
func ShellQuote(path string) string {
	if runtime.GOOS == "windows" {
		return `"` + path + `"`
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// PowerShellQuote wraps a path for safe embedding inside a PowerShell -Command string.
// Uses PowerShell single-quote syntax where ' is escaped as ”.
func PowerShellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "''") + "'"
}
