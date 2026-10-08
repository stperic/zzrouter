package servercli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
	"gopkg.in/yaml.v3"
)

// ensureConfigStructure ensures all required config files and directories exist
// onlyMissing: if true, only creates missing files (doesn't overwrite existing)
// silent: if true, doesn't print messages
// Returns true if any files were created
func ensureConfigStructure(onlyMissing bool, silent bool) (bool, error) {
	paths := pkgConfig.Paths()

	// Create all required directories using PathResolver
	if err := paths.EnsureAllDirs(); err != nil {
		return false, fmt.Errorf("failed to create directories: %w", err)
	}

	createdAny := false

	// Get paths from PathResolver (single source of truth)
	configPath := paths.GetNodeConfigPath()
	appsDir := paths.GetAppsConfigDir()
	envPath := paths.GetEnvFilePath()
	gitignorePath := filepath.Join(paths.GetConfigDir(), ".gitignore")

	// 2. Install per-provider directory tree if missing or force.
	// InstallDefaults is copy-if-missing per file so it's safe to call on
	// upgrade — user edits on existing files are preserved.
	appsDirExists := false
	if info, err := os.Stat(appsDir); err == nil && info.IsDir() {
		appsDirExists = true
	}
	if !onlyMissing || !appsDirExists {
		if err := templates.InstallDefaults(appsDir); err != nil {
			return createdAny, fmt.Errorf("install providers templates: %w", err)
		}
		createdAny = true
	}

	// 3. Create node.yaml with default hostname if missing or force
	if !onlyMissing || !fileExists(configPath) {
		defaultNodename := getDefaultNodename()
		if err := createConfigFileWithNodename(configPath, defaultNodename); err != nil {
			return createdAny, fmt.Errorf("failed to create configuration file: %w", err)
		}
		createdAny = true
	}

	// 6. Create .env file if missing or force
	if !onlyMissing || !fileExists(envPath) {
		if err := createEnvFile(envPath); err != nil {
			return createdAny, fmt.Errorf("failed to create .env file: %w", err)
		}
		createdAny = true
	}

	// 7. Create .gitignore if missing or force
	if !onlyMissing || !fileExists(gitignorePath) {
		if err := createGitignore(gitignorePath); err != nil {
			if !silent {
				fmt.Printf(" Warning: failed to create .gitignore: %v\n", err)
			}
		}
		createdAny = true
	}

	return createdAny, nil
}

// checkAndAskToStopRunningNode checks if zzrouter server is running and asks user permission to stop it
// Returns true if server was actually stopped, false if it wasn't running or user declined
func checkAndAskToStopRunningNode() (bool, error) {
	// Load config to get the server port
	cm := pkgConfig.NewConfigManager("zzrouter")
	configPath := cm.GetNodeConfigPath()

	hostConfig, err := pkgConfig.LoadNodeConfig(configPath)
	if err != nil {
		// If config doesn't exist, assume no server is running
		return false, nil //nolint:nilerr // soft "server not running" signal; init continues
	}

	// Check if server is running
	if checkNodeRunning(hostConfig.Node.Port) {
		fmt.Println(" zzRouter server is currently running.")
		fmt.Println("   Configuration changes require the server to be stopped.")
		fmt.Println()
		fmt.Print("Stop the server now? [y/N]: ")

		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			fmt.Println("Stopping zzRouter server...")
			// killExistingServeProcesses returns nil when no process is found —
			// any non-nil error here is a real failure (process listing or kill).
			if err := killExistingServeProcesses(); err != nil {
				return false, err
			}
			fmt.Println("✓ Node stopped successfully.")
			return true, nil
		} else {
			fmt.Println("Configuration cancelled. Node remains running.")
			return false, fmt.Errorf("user declined to stop server")
		}
	}

	return false, nil
}

// checkAndStopRunningNode checks if zzrouter server is running and stops it (DRY)
// Returns true if server was actually stopped, false if it wasn't running.
//
// Note: killExistingServeProcesses returns nil whether or not a process was
// found, so we can't actually distinguish "stopped" from "wasn't running" here.
// Returning true unconditionally on success preserves prior behavior; callers
// that need the distinction should query process state directly.
func checkAndStopRunningNode() (bool, error) {
	if err := killExistingServeProcesses(); err != nil {
		return false, err
	}
	return true, nil
}

func runNodeInit(force bool) error {
	// Check if zzrouter server is running and stop it if needed
	serverWasStopped, err := checkAndStopRunningNode()
	if err != nil {
		return err
	}
	_ = serverWasStopped // Will be used later if needed

	// Use PathResolver for all paths (single source of truth)
	paths := pkgConfig.Paths()
	cm := pkgConfig.NewConfigManager("zzrouter")
	outputDir := paths.GetConfigDir()

	// Ensure config directory exists
	if err := paths.EnsureConfigDir(); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Get absolute path for display
	absPath, err := filepath.Abs(outputDir)
	if err != nil {
		absPath = outputDir
	}

	// Define file paths using PathResolver
	configPath := paths.GetNodeConfigPath()
	appsDir := paths.GetAppsConfigDir()

	// Check if files already exist
	configExists := false
	appsExists := false
	isTemplateConfig := false

	if fileExists(configPath) {
		configExists = true
		// Check if it's just a template config (empty server name)
		if cfg, err := cm.LoadNodeConfigFromDir(outputDir); err == nil {
			if cfg.Node.Name == "" {
				isTemplateConfig = true
			}
		}
	}
	if info, err := os.Stat(appsDir); err == nil && info.IsDir() {
		appsExists = true
	}

	// If config exists and not force, offer to recreate or update
	// But skip the prompt if it's just a template config (empty server name)
	if (configExists || appsExists) && !force && !isTemplateConfig {
		fmt.Printf("Existing host configuration detected in:\n %s...\n", absPath)
		fmt.Println()
		fmt.Print("Do you want to recreate your configuration? [y/N]: ")

		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			fmt.Println()
			fmt.Println("Recreating configuration...")
			// Continue with fresh init (same as --force behavior)
		} else {
			fmt.Println()
			fmt.Println("Configuration preserved.")
			fmt.Println("Use 'zzrouter-node config' to update existing configuration.")
			return nil
		}
	} else {
		fmt.Printf("Initializing zzRouter host configuration in:\n %s...\n", absPath)
	}

	checkPlatformEnvironment(outputDir)

	ctx, cancel := context.WithTimeout(context.Background(), constants.HTTPShortTimeout)
	defer cancel()

	// PHASE 1: Create ALL directories first (before any prompts)
	// Uses PathResolver.EnsureAllDirs() for consistent path handling
	if err := paths.EnsureAllDirs(); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// 2. Install per-provider directory tree (all apps disabled by default).
	// Copy-if-missing per file — safe to re-run on existing installs.
	if err := templates.InstallDefaults(appsDir); err != nil {
		return fmt.Errorf("install providers templates: %w", err)
	}

	// 3. Create host configuration file with system hostname
	defaultNodename := getDefaultNodename()
	if err := createConfigFileWithNodename(configPath, defaultNodename); err != nil {
		return fmt.Errorf("failed to create configuration file: %w", err)
	}

	// 4. Create .env file
	envPath := paths.GetEnvFilePath()
	if err := createEnvFile(envPath); err != nil {
		return fmt.Errorf("failed to create .env file: %w", err)
	}

	// 5. Create .gitignore
	gitignorePath := filepath.Join(paths.GetConfigDir(), ".gitignore")
	if err := createGitignore(gitignorePath); err != nil {
		fmt.Printf(" Warning: failed to create .gitignore: %v\n", err)
	}
	fmt.Println()

	// PHASE 2: Read config and prompt for updates
	// 5. Prompt for hostname (read existing, update if needed)
	hostname := promptForNodenameUpdate(configPath)
	if hostname != "" {
		if err := updateNodenameInConfig(configPath, hostname); err != nil {
			fmt.Printf(" Warning: failed to update hostname: %v\n", err)
		}
	}
	fmt.Println()

	// Run app discovery and configuration
	fmt.Println("Now let's configure your inference providers...")
	fmt.Println("Inference providers are required to serve LLMs.")
	fmt.Println("At least one provider must be enabled for zzRouter to function.")
	fmt.Println()
	fmt.Println("Supported providers: Ollama, vLLM, llama.cpp, MLX")
	fmt.Println()
	fmt.Print("Press Enter to continue...")
	_, _ = fmt.Scanln()
	fmt.Println()

	// Load the providers/ directory we just installed.
	appsConfig, err := pkgConfig.LoadAppsConfig(appsDir)
	if err != nil {
		return fmt.Errorf("failed to load providers config: %w", err)
	}

	// Run provider discovery and auto-configuration
	providerConfigs, err := RunProviderConfigurator(ctx, appsConfig, false)
	if err != nil {
		return fmt.Errorf("provider discovery failed: %w", err)
	}

	// Persist discovery results into the providers/ directory tree.
	if providerConfigs != nil {
		if err := updateAppsYAML(appsDir, providerConfigs); err != nil {
			return fmt.Errorf("failed to update providers config: %w", err)
		}
		fmt.Println()
		fmt.Println("Provider configuration saved to providers/")
	} else {
		fmt.Println()
		fmt.Println("No providers configured. You can configure them later by editing files under providers/")
	}

	// Initialization complete
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  1. Start zzRouter server: zzrouter-node start")
	fmt.Println("  2. Configure cluster networking: zzrouter-node config cluster ")
	fmt.Println()

	return nil
}

// promptForNodenameUpdate reads existing hostname from config and prompts for update
func promptForNodenameUpdate(configPath string) string {
	// Get system hostname as default
	systemNodename := getDefaultNodename()

	// Read existing hostname from config (might be empty from template)
	existingNodename := readNodenameFromConfig(configPath)
	if existingNodename == "" {
		existingNodename = systemNodename
	}

	fmt.Printf("Current hostname: %s\n", existingNodename)
	fmt.Println()
	fmt.Print("Enter new hostname (or press Enter to keep current): ")

	var input string
	_, _ = fmt.Scanln(&input)
	input = strings.TrimSpace(input)

	if input == "" {
		fmt.Printf("Keeping hostname: %s\n", existingNodename)
		return "" // No change needed
	}

	// Validate and clean input
	input = strings.ToLower(input)
	input = strings.ReplaceAll(input, " ", "-")
	input = strings.ReplaceAll(input, "_", "-")

	fmt.Printf("Updating hostname to: %s\n", input)
	return input
}

// getDefaultNodename returns a default hostname based on system hostname
func getDefaultNodename() string {
	// Try to get system hostname first
	if sysNodename, err := os.Hostname(); err == nil && sysNodename != "" {
		// Clean up hostname (remove domain, make lowercase, replace dots/spaces)
		hostname := strings.ToLower(sysNodename)
		hostname = strings.Split(hostname, ".")[0] // Remove domain
		hostname = strings.ReplaceAll(hostname, " ", "-")
		hostname = strings.ReplaceAll(hostname, "_", "-")
		return hostname
	}

	// Fallback only if system hostname is unavailable
	return "zzrouter"
}

// readNodenameFromConfig reads the hostname from node.yaml
func readNodenameFromConfig(configPath string) string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}

	var config struct {
		Node struct {
			Name string `yaml:"name"`
		} `yaml:"server"`
	}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}

	return config.Node.Name
}

// updateNodenameInConfig updates the hostname in node.yaml
func updateNodenameInConfig(configPath, hostname string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	// Pre-process YAML to fix Windows path escape issues
	data = pkgConfig.FixWindowsPathsInYAML(data)

	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}

	// Update server.name
	if server, ok := config["server"].(map[string]any); ok {
		server["name"] = hostname
	}

	// Write back
	output, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	// Fix Windows paths in output before writing
	output = pkgConfig.FixWindowsPathsInYAML(output)

	// 0o600 is mandatory: pkg/config.requireSafePerms refuses to load
	// node.yaml at any wider mode (commit f41552a). os.WriteFile only
	// applies the mode on file creation — for the upgrade case where
	// a previously-loose file is being overwritten, the helper's
	// chmod-then-rename forces the new mode regardless of prior state.
	return utils.AtomicWriteFile(configPath, output, 0o600)
}

// createConfigFileWithNodename creates the host config file with a custom hostname
func createConfigFileWithNodename(path string, hostname string) error {
	content := templates.GetNodeTemplate()

	// Replace the empty hostname with the custom one
	// The template might have comments after the value, so match more flexibly
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		// Match lines that have 'name: ""' with optional comments after
		if strings.Contains(line, "name:") && strings.Contains(line, "\"\"") {
			// Replace the empty quotes with the hostname
			lines[i] = strings.Replace(line, "\"\"", fmt.Sprintf("\"%s\"", hostname), 1)
			break
		}
	}
	content = strings.Join(lines, "\n")

	// 0o600: same loader gate as updateNodenameInConfig above. The
	// helper makes the perm idempotent across re-runs of init.
	return utils.AtomicWriteFile(path, []byte(content), 0o600)
}

func createGitignore(path string) error {
	content := `# zzRouter directories
models/
cache/
logs/
*.log

# Configuration (may contain sensitive data)
# Uncomment if you want to ignore config
# node.yaml
# cli.yaml

# Provider configuration (user-specific)
providers/
.env

# Podman
.env
`
	return os.WriteFile(path, []byte(content), 0644)
}

// updateAppsYAML writes the providers/ directory tree from an AppsConfig.
// SaveToDir handles atomic per-file writes and Windows path escaping.
func updateAppsYAML(dir string, config *pkgConfig.AppsConfig) error {
	return config.SaveToDir(dir)
}

// envKeyRandomHex generates a 24-byte random hex string (48 chars) used as
// the entropy portion of a generated zzRouter API key.
func envKeyRandomHex() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand on a functioning OS will not fail; if it does, we
		// cannot safely generate keys and startup should abort rather than
		// silently installing a weak fallback.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "zzr_" + hex.EncodeToString(buf)
}

// createEnvFile writes a .env file populated from the embedded env-default
// template with freshly generated ADMIN and CLUSTER_NETWORK keys filled in.
// This matches install.sh / install.ps1 so `make bc` (which bypasses the
// installer scripts) produces a self-consistent config that works with the
// coordinator-mode default in node.yaml.
func createEnvFile(path string) error {
	admin := envKeyRandomHex()
	cluster := envKeyRandomHex()

	content := templates.EnvTemplate
	if content == "" {
		// Defensive: the template is //go:embed'd, so empty means build
		// corruption. Fall back to a minimal viable .env rather than
		// writing something that fails coordinator startup.
		content = fmt.Sprintf(
			"ZZROUTER_ADMIN_API_KEY=%s\nZZROUTER_CLUSTER_NETWORK_KEY=%s\n",
			admin, cluster)
	} else {
		content = regexp.MustCompile(`(?m)^#\s*ZZROUTER_ADMIN_API_KEY=\s*$`).
			ReplaceAllString(content, "ZZROUTER_ADMIN_API_KEY="+admin)
		content = regexp.MustCompile(`(?m)^#\s*ZZROUTER_CLUSTER_NETWORK_KEY=\s*$`).
			ReplaceAllString(content, "ZZROUTER_CLUSTER_NETWORK_KEY="+cluster)
	}

	// 0600 — .env contains secrets that must not be world-readable.
	return os.WriteFile(path, []byte(content), 0600)
}

// checkPlatformEnvironment performs platform-specific checks and provides recommendations
func checkPlatformEnvironment(outputDir string) {
	// Check if running as root
	if os.Getuid() == 0 {
		// Check if using a system directory (these are acceptable for root)
		isSystemDir := strings.HasPrefix(outputDir, "/opt/") ||
			strings.HasPrefix(outputDir, "/usr/local/") ||
			strings.HasPrefix(outputDir, "/etc/") ||
			strings.HasPrefix(outputDir, "/var/")

		// Only warn if using /root directory (not system directories)
		if strings.HasPrefix(outputDir, "/root") {
			fmt.Println()
			fmt.Println(" WARNING: Running as root with config in /root directory")
			fmt.Println()
			fmt.Println("📁 Installing in /root directory")
			fmt.Println()
			fmt.Println(" RECOMMENDATION: Use a system directory instead")
			fmt.Println()
			fmt.Println("   Better options:")
			fmt.Println("   1. System directory (recommended for servers):")
			fmt.Println("      $ mkdir -p /opt/zzrouter && cd /opt/zzrouter")
			fmt.Println("      $ zzrouter-node config init --config-dir /opt/zzrouter")
			fmt.Println()
			fmt.Println("   2. Non-root user (recommended for development):")
			fmt.Println("      $ useradd -m zzrouter && su - zzrouter")
			fmt.Println("      $ zzrouter-node config init")
			fmt.Println()

			// Platform-specific warnings (only for /root directory)
			switch runtime.GOOS {
			case "linux":
				fmt.Println("🐧 Linux Platform Notes:")
				fmt.Println("   • Running as root may cause nested container issues")
				fmt.Println("   • LXC containers: Use non-root user to avoid RLIMIT errors")
				fmt.Println()
			case "darwin":
				fmt.Println("🍎 macOS Platform Notes:")
				fmt.Println("   • Running as root is less common on macOS")
				fmt.Println("   • Consider using your regular user account")
				fmt.Println()
			}

			// Ask for confirmation
			fmt.Print("Continue with current setup? (y/N): ")
			var response string
			_, _ = fmt.Scanln(&response)
			if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
				fmt.Println("Initialization cancelled.")
				os.Exit(0)
			}
			fmt.Println()
		} else if isSystemDir {
			// System directory with root is fine - just a brief informational message
			fmt.Println()
			fmt.Printf("✓ Installing in system directory: %s\n", outputDir)
			fmt.Println()
		}
	}
}
