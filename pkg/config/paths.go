// Package config provides OS-agnostic configuration file management for zzRouter
package config

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	"github.com/kirsle/configdir"
)

// envTestHomeOverride, when set, redirects the entire path tree (config,
// data, cache, logs) under a single directory. Intended SOLELY for
// integration tests to keep writes inside t.TempDir() instead of clobbering
// the user's real XDG config; production deployments should not rely on it.
// Layout under the override root:
//
//	$ZZROUTER_TEST_HOME/           → config + data
//	$ZZROUTER_TEST_HOME/cache/     → cache
//	$ZZROUTER_TEST_HOME/logs/      → logs
const envTestHomeOverride = "ZZROUTER_TEST_HOME"

// envConfigDirOverride pins the config root: node.yaml, providers/, .env,
// and the cluster material (ca/, identity/, cluster/) that
// clusternode.PathsFromConfigDir derives from it.
//
// It is what lets a node run as a system service and still read the
// operator's config. A service runs as its own account, so every
// account-relative default resolves somewhere the operator never looks —
// and the symptom is not "permission denied" but a node that silently
// runs a different, freshly-defaulted config. Setting this machine-wide
// gives both the service and the CLI one answer.
//
// Config only. Data, cache and logs stay account-relative because they
// are per-run artifacts rather than the identity of the node.
const envConfigDirOverride = "ZZROUTER_CONFIG_DIR"

// PathResolver is the single source of truth for all zzRouter paths. Paths
// are resolved lazily on each access so env-var overrides set after
// construction (notably t.Setenv in tests) take effect.
type PathResolver struct {
	appName string
}

// pathLayout holds the resolved directory paths for a single run context.
// Each field is an absolute path. Resolved once, then read-only.
//
// Platform conventions:
//
//	Linux (user):    config ~/.config/zzrouter  data ~/.local/share/zzrouter  cache ~/.cache/zzrouter           logs <data>/logs
//	Linux (service): config /etc/zzrouter       data /var/lib/zzrouter        cache /var/lib/zzrouter/cache      logs /var/log/zzrouter
//	Linux (root):    config /opt/zzrouter       data /opt/zzrouter            cache /opt/zzrouter/cache          logs /opt/zzrouter/logs
//	macOS:           config ~/Library/Application Support/zzrouter            cache ~/Library/Caches/zzrouter    logs ~/Library/Logs/zzrouter
//	Windows:         config %APPDATA%\zzrouter  data %LOCALAPPDATA%\zzrouter  cache <data>\cache                 logs <data>\logs
type pathLayout struct {
	config string // small files: node.yaml, .env, and providers/ subdir tree
	data   string // large persistent: models, providers, PIDs
	cache  string // expendable, safe to delete
	logs   string // potentially large, machine-specific
}

// NewPathResolver creates a new PathResolver instance.
func NewPathResolver(appName string) *PathResolver {
	return &PathResolver{appName: appName}
}

// Subdirectory names (single source of truth)
const (
	SubdirModels = "models"
	SubdirLogs   = "logs"
	SubdirCache  = "cache"
	SubdirPIDs   = "pids"

	// SubdirVersions and SubdirBin name the two halves of a managed
	// install: one directory per installed version, and the symlinks
	// that say which of them is running. Here rather than in pkg/update
	// because pkg/service creates these directories and cannot import
	// pkg/update, which imports it. See pkg/update/layout.go.
	SubdirVersions = "versions"
	SubdirBin      = "bin"
)

// Linux FHS-compliant paths (used by service and root modes)
const (
	LinuxConfigDir = "/etc/zzrouter"
	LinuxDataDir   = "/var/lib/zzrouter"
	LinuxLogDir    = "/var/log/zzrouter"
	LinuxRootDir   = "/opt/zzrouter"
)

// --- Run Context Detection ---

func (p *PathResolver) IsRoot() bool {
	return os.Getuid() == 0
}

func (p *PathResolver) IsServiceUser() bool {
	if os.Getenv("USER") == "zzrouter" {
		return true
	}
	currentUser, err := user.Current()
	if err != nil {
		return false
	}
	return currentUser.Username == "zzrouter"
}

// --- Layout Resolution (per access, env-aware) ---

func (p *PathResolver) resolve() pathLayout {
	return p.buildLayout()
}

func (p *PathResolver) buildLayout() pathLayout {
	if root := os.Getenv(envTestHomeOverride); root != "" {
		return p.buildTestOverrideLayout(root)
	}
	layout := p.buildPlatformLayout()
	// Applied after the platform layout so the override wins over every
	// account-relative default, on every platform.
	if dir := os.Getenv(envConfigDirOverride); dir != "" {
		layout.config = dir
	}
	return layout
}

func (p *PathResolver) buildPlatformLayout() pathLayout {
	switch runtime.GOOS {
	case "linux":
		return p.buildLinuxLayout()
	case "darwin":
		return p.buildDarwinLayout()
	case "windows":
		return p.buildWindowsLayout()
	default:
		return p.buildFallbackLayout()
	}
}

// buildTestOverrideLayout pins every directory under a single root. Only
// fires when ZZROUTER_TEST_HOME is set — see envTestHomeOverride.
func (p *PathResolver) buildTestOverrideLayout(root string) pathLayout {
	return pathLayout{
		config: root,
		data:   root,
		cache:  filepath.Join(root, SubdirCache),
		logs:   filepath.Join(root, SubdirLogs),
	}
}

func (p *PathResolver) buildLinuxLayout() pathLayout {
	if p.IsRoot() {
		configDir := LinuxRootDir
		if info, err := os.Stat(LinuxConfigDir); err == nil && info.IsDir() {
			configDir = LinuxConfigDir
		}
		return pathLayout{
			config: configDir,
			data:   LinuxRootDir,
			cache:  filepath.Join(LinuxRootDir, SubdirCache),
			logs:   filepath.Join(LinuxRootDir, SubdirLogs),
		}
	}
	if p.IsServiceUser() {
		return pathLayout{
			config: LinuxConfigDir,
			data:   LinuxDataDir,
			cache:  filepath.Join(LinuxDataDir, SubdirCache),
			logs:   LinuxLogDir,
		}
	}

	// Regular user: XDG base directories
	home, _ := os.UserHomeDir()
	if home == "" {
		return p.buildFallbackLayout()
	}

	xdgConfig := filepath.Join(home, ".config", p.appName)
	xdgData := filepath.Join(home, ".local", "share", p.appName)
	xdgCache := filepath.Join(home, ".cache", p.appName)

	if override := os.Getenv("XDG_DATA_HOME"); override != "" {
		xdgData = filepath.Join(override, p.appName)
	}

	return pathLayout{
		config: xdgConfig,
		data:   xdgData,
		cache:  xdgCache,
		logs:   filepath.Join(xdgData, SubdirLogs),
	}
}

func (p *PathResolver) buildDarwinLayout() pathLayout {
	home, _ := os.UserHomeDir()
	if home == "" {
		return p.buildFallbackLayout()
	}

	appSupport := filepath.Join(home, "Library", "Application Support", p.appName)
	if override := os.Getenv("XDG_DATA_HOME"); override != "" {
		appSupport = filepath.Join(override, p.appName)
	}

	return pathLayout{
		config: appSupport,
		data:   appSupport,
		cache:  filepath.Join(home, "Library", "Caches", p.appName),
		logs:   filepath.Join(home, "Library", "Logs", p.appName),
	}
}

func (p *PathResolver) buildWindowsLayout() pathLayout {
	roaming := os.Getenv("APPDATA")
	local := os.Getenv("LOCALAPPDATA")
	if roaming == "" || local == "" {
		return p.buildFallbackLayout()
	}

	dataDir := filepath.Join(local, p.appName)
	return pathLayout{
		config: filepath.Join(roaming, p.appName),
		data:   dataDir,
		cache:  filepath.Join(dataDir, SubdirCache),
		logs:   filepath.Join(dataDir, SubdirLogs),
	}
}

func (p *PathResolver) buildFallbackLayout() pathLayout {
	configDir := configdir.LocalConfig(p.appName)
	cacheDir := configdir.LocalCache(p.appName)
	return pathLayout{
		config: configDir,
		data:   configDir,
		cache:  cacheDir,
		logs:   filepath.Join(configDir, SubdirLogs),
	}
}

// --- Public Directory Getters ---

func (p *PathResolver) GetConfigDir() string { return p.resolve().config }
func (p *PathResolver) GetDataDir() string   { return p.resolve().data }
func (p *PathResolver) GetCacheDir() string  { return p.resolve().cache }
func (p *PathResolver) GetLogsDir() string   { return p.resolve().logs }

func (p *PathResolver) GetPIDDir() string    { return filepath.Join(p.resolve().data, SubdirPIDs) }
func (p *PathResolver) GetModelsDir() string { return filepath.Join(p.resolve().data, SubdirModels) }

// --- Config File Paths ---

func (p *PathResolver) GetNodeConfigPath() string {
	return filepath.Join(p.resolve().config, "node.yaml")
}
func (p *PathResolver) GetAppsConfigDir() string {
	return filepath.Join(p.resolve().config, "providers")
}
func (p *PathResolver) GetEnvFilePath() string { return filepath.Join(p.resolve().config, ".env") }

// --- Directory Creation ---

func (p *PathResolver) EnsureConfigDir() error {
	return configdir.MakePath(p.GetConfigDir())
}

func (p *PathResolver) EnsureAllDirs() error {
	dirs := []string{
		p.GetConfigDir(),
		p.GetDataDir(),
		p.GetModelsDir(),
		p.GetLogsDir(),
		p.GetCacheDir(),
	}

	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}

// --- Default PathResolver ---

var defaultPathResolver = NewPathResolver("zzrouter")

func Paths() *PathResolver {
	return defaultPathResolver
}
