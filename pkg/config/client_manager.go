// Package config provides OS-agnostic configuration file management for zzRouter
// Client configuration management functionality
package config

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/viper"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
	"gopkg.in/yaml.v3"
)

// LoadClientConfig loads client configuration from file
// Searches in priority order: current directory, then user config directory
func (cm *ConfigManager) LoadClientConfig() (*ClientConfig, error) {
	// Load .env from config directory so env var references (e.g. ZZROUTER_ADMIN_API_KEY) resolve
	cm.LoadConfigDirEnv()

	// Try to find existing config file (current dir first, then user config)
	configFile := cm.FindClientConfigFile()

	// If no config found, create default in user config directory
	if configFile == "" {
		configDir := cm.GetClientConfigDir()
		if err := cm.EnsureDir(configDir); err != nil {
			return nil, fmt.Errorf("failed to create client config dir: %w", err)
		}

		configFile = filepath.Join(configDir, clientConfigFileName)

		// Write template directly to file - this is the source of truth
		templateContent := templates.GetClientTemplate()
		if err := os.WriteFile(configFile, []byte(templateContent), 0600); err != nil {
			return nil, fmt.Errorf("failed to write template to file: %w", err)
		}
	}

	// Load config from file (either existing or newly created template)
	v := viper.New()
	v.SetConfigFile(configFile)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read client config from %s: %w", configFile, err)
	}

	var config ClientConfig
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal client config: %w", err)
	}

	// Merge with defaults for any missing fields (e.g., new search settings added in updates)
	config = *cm.mergeWithDefaults(&config)

	return &config, nil
}

// SaveClientConfig saves client configuration to file.
// Uses proper YAML marshal instead of regex text replacement.
func (cm *ConfigManager) SaveClientConfig(config *ClientConfig) error {
	configDir := cm.GetClientConfigDir()
	if err := cm.EnsureDir(configDir); err != nil {
		return fmt.Errorf("failed to create client config dir: %w", err)
	}

	configFile := filepath.Join(configDir, clientConfigFileName)

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal client config: %w", err)
	}

	return os.WriteFile(configFile, data, 0600)
}

// mergeWithDefaults merges loaded config with defaults for any missing fields
func (cm *ConfigManager) mergeWithDefaults(config *ClientConfig) *ClientConfig {
	// Get defaults from template
	templateContent := templates.GetClientTemplate()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader([]byte(templateContent))); err != nil {
		// If template fails, use minimal hardcoded defaults
		return config
	}

	var defaults ClientConfig
	if err := v.Unmarshal(&defaults); err != nil {
		// If unmarshal fails, use minimal hardcoded defaults
		return config
	}

	// Merge preferences
	if config.Preferences.Timeout == 0 {
		config.Preferences.Timeout = defaults.Preferences.Timeout
	}
	if config.Preferences.OutputFormat == "" {
		config.Preferences.OutputFormat = defaults.Preferences.OutputFormat
	}

	// Merge chat settings (apply template defaults for new installs)
	if config.Preferences.Chat.Temperature == 0 && defaults.Preferences.Chat.Temperature > 0 {
		config.Preferences.Chat.Temperature = defaults.Preferences.Chat.Temperature
	}
	// TopP defaults to -1 (not sent). Only apply default if config has zero value
	// and template default is positive (for backwards compat with old templates).
	if config.Preferences.Chat.TopP == 0 && defaults.Preferences.Chat.TopP > 0 {
		config.Preferences.Chat.TopP = defaults.Preferences.Chat.TopP
	}

	// Merge search settings (add defaults if missing)
	if len(config.Preferences.Search.ModelTags) == 0 {
		config.Preferences.Search.ModelTags = defaults.Preferences.Search.ModelTags
	}
	if config.Preferences.Search.DefaultLogic == "" {
		config.Preferences.Search.DefaultLogic = defaults.Preferences.Search.DefaultLogic
	}

	// Migrate deprecated provider_prefs → registry_prefs
	if config.Preferences.Search.DefaultRegistry == "" && config.Preferences.Search.DefaultProvider != "" {
		config.Preferences.Search.DefaultRegistry = config.Preferences.Search.DefaultProvider
		config.Preferences.Search.DefaultProvider = ""
	}
	if config.Preferences.Search.RegistryPrefs == nil && config.Preferences.Search.ProviderPrefs != nil {
		config.Preferences.Search.RegistryPrefs = config.Preferences.Search.ProviderPrefs
		config.Preferences.Search.ProviderPrefs = nil
	}

	return config
}

// GetNodeConfig returns a ClientNodeConfig for the host from client config
// Returns an error if no host is configured - auto-discovery should only happen in signin command
func (cc *ClientConfig) GetNodeConfig() (*ClientNodeConfig, error) {
	// Check if host is configured
	if cc.Node.Address == "" {
		return nil, fmt.Errorf("no host configured")
	}

	// Node is configured, return it
	scheme := "http"
	if cc.Node.Secure {
		scheme = "https"
	}

	apiKey, _ := resolveAPIKey(cc.Node.Key, cc.Node.APIKey)
	return &ClientNodeConfig{
		Name:      formatNodeNameForConfig(cc.Node.Address, cc.Node.Port),
		Address:   fmt.Sprintf("%s://%s:%d", scheme, cc.Node.Address, cc.Node.Port),
		APIKey:    apiKey,
		TLSCACert: cc.Node.TLSCACert,
	}, nil
}

// GetDefaultNodeConfig returns a ClientNodeConfig for the default host (same as GetNodeConfig since there's only one)
func (cc *ClientConfig) GetDefaultNodeConfig() (*ClientNodeConfig, error) {
	return cc.GetNodeConfig()
}

// GetNodeConfigWithAutoDiscovery returns a ClientNodeConfig with auto-discovery fallback
// This should ONLY be used by the signin command - all other commands should use GetNodeConfig()
func (cc *ClientConfig) GetNodeConfigWithAutoDiscovery() (*ClientNodeConfig, error) {
	// Step 1: Check if host is configured
	if cc.Node.Address != "" {
		// Node is configured, return it
		scheme := "http"
		if cc.Node.Secure {
			scheme = "https"
		}

		apiKey, _ := resolveAPIKey(cc.Node.Key, cc.Node.APIKey)
		return &ClientNodeConfig{
			Name:      formatNodeNameForConfig(cc.Node.Address, cc.Node.Port),
			Address:   fmt.Sprintf("%s://%s:%d", scheme, cc.Node.Address, cc.Node.Port),
			APIKey:    apiKey,
			TLSCACert: cc.Node.TLSCACert,
		}, nil
	}

	// Step 2: No host configured, try localhost detection
	fmt.Println("No host configured, checking for running host on localhost...")

	detectedNode, err := detectLocalhostNode()
	if err == nil && detectedNode != nil {
		fmt.Printf("Found running zzRouter server at %s\n", detectedNode.Address)
		fmt.Println()

		// Preserve the API key from current client config
		apiKey, _ := resolveAPIKey(cc.Node.Key, cc.Node.APIKey)
		detectedNode.APIKey = apiKey

		// Prompt user to save this configuration
		if promptToSaveNode(detectedNode) {
			port := getPortFromAddress(detectedNode.Address)
			if saveErr := saveDetectedNode("localhost", port, false); saveErr != nil {
				fmt.Printf(" Warning: Could not save host configuration: %v\n", saveErr)
			} else {
				fmt.Println("Node configuration saved")
			}
		}
		fmt.Println()

		return detectedNode, nil
	}

	fmt.Println("No zzRouter host found on localhost")
	fmt.Println()

	// Step 3: Try mDNS discovery (future)
	// TODO: Implement mDNS discovery
	// fmt.Println("Searching for zzRouter nodes on local network...")

	// Step 4: Prompt for manual endpoint entry
	return promptForManualNodeEntry()
}

// LoadClientConfig convenience function that creates a ConfigManager and loads client config
func LoadClientConfig(appName string) (*ClientConfig, error) {
	cm := NewConfigManager(appName)
	return cm.LoadClientConfig()
}

// detectLocalhostNode tries to find a running zzrouter host on localhost by checking common ports
func detectLocalhostNode() (*ClientNodeConfig, error) {
	// Common ports to check for zzRouter nodes
	commonPorts := DefaultCommonPorts

	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}

	for _, port := range commonPorts {
		// Try unauthenticated health endpoint first
		healthURL := fmt.Sprintf("http://localhost:%d/health", port)

		// TODO: Thread context from callers for proper cancellation support
		req, err := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
		if err != nil {
			continue // Try next port
		}

		resp, err := client.Do(req)
		if err != nil {
			continue // Try next port
		}
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			// Found a running zzrouter host!
			return &ClientNodeConfig{
				Name:    "localhost",
				Address: fmt.Sprintf("http://localhost:%d", port),
			}, nil
		}
	}

	return nil, fmt.Errorf("no running zzrouter host found on localhost")
}

// Node Name Standardization for Config
// ====================================

// formatNodeNameForConfig formats host names for configuration display
func formatNodeNameForConfig(hostName string, port int) string {
	// For localhost, only show port if it's not standard
	if hostName == "localhost" {
		if port != 0 && port != 80 && port != 443 && port != DefaultZZROUTERPort {
			return fmt.Sprintf("localhost:%d", port)
		}
		return "localhost"
	}

	// For other hosts, include port if not standard
	if port != 0 && port != 80 && port != 443 {
		return fmt.Sprintf("%s:%d", hostName, port)
	}

	return hostName
}

// getPortFromAddress extracts the port number from a host address like "http://localhost:9090"
func getPortFromAddress(address string) int {
	// Simple parsing - assumes format "http://hostname:port"
	if len(address) > 10 && address[:7] == "http://" {
		// Find the last colon
		for i := len(address) - 1; i >= 0; i-- {
			if address[i] == ':' {
				// Try to parse the port number
				portStr := address[i+1:]
				if port, err := strconv.Atoi(portStr); err == nil {
					return port
				}
				break
			}
		}
	}
	return DefaultZZROUTERPort // Default fallback
}

// promptToSaveNode asks the user if they want to save the detected host configuration
func promptToSaveNode(host *ClientNodeConfig) bool {
	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("Would you like to save %s as your default host? (Y/n): ", host.Address)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	response = strings.ToLower(strings.TrimSpace(response))

	// Default to "yes" if user just presses Enter
	if response == "" || response == "y" || response == "yes" {
		return true
	}

	return false
}

// saveDetectedNode saves a detected host configuration
func saveDetectedNode(address string, port int, secure bool) error {
	cm := NewConfigManager("zzrouter")
	store, err := NewClientConfigStore(cm)
	if err != nil {
		// If load fails, save a fresh config with just connection details.
		// This handles first-run where cli.yaml doesn't exist yet.
		config := &ClientConfig{}
		config.Node.Address = address
		config.Node.Port = port
		config.Node.Secure = secure
		return cm.SaveClientConfig(config)
	}
	return store.SetNodeConnection(address, port, secure)
}

// promptForManualNodeEntry prompts the user to manually enter a host endpoint
func promptForManualNodeEntry() (*ClientNodeConfig, error) {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Manual Node Configuration")
	fmt.Println("============================")
	fmt.Println()

	// Prompt for address
	fmt.Print("Enter host address (e.g., 192.168.1.100 or example.com): ")
	address, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read address: %w", err)
	}
	address = strings.TrimSpace(address)

	if address == "" {
		return nil, fmt.Errorf("address cannot be empty")
	}

	// Prompt for port
	fmt.Print("Enter port number (default: 9090): ")
	portStr, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read port: %w", err)
	}
	portStr = strings.TrimSpace(portStr)

	port := DefaultZZROUTERPort
	if portStr != "" {
		parsedPort, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port number: %w", err)
		}
		port = parsedPort
	}

	// Prompt for HTTPS
	fmt.Print("Use HTTPS? (y/N): ")
	httpsStr, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read HTTPS preference: %w", err)
	}
	httpsStr = strings.ToLower(strings.TrimSpace(httpsStr))
	useHTTPS := httpsStr == "y" || httpsStr == "yes"

	// Test the connection
	fmt.Printf("\nTesting connection to %s:%d...\n", address, port)

	scheme := "http"
	if useHTTPS {
		scheme = "https"
	}

	healthURL := fmt.Sprintf("%s://%s:%d/health", scheme, address, port)
	client := &http.Client{Timeout: constants.ClusterHealthCheckTimeout}

	// TODO: Thread context from callers for proper cancellation support
	req, err := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
	if err == nil {
		resp, err := client.Do(req)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode == http.StatusOK {
				fmt.Println("Connection successful!")
			} else {
				fmt.Printf(" Node responded with status %d\n", resp.StatusCode)
			}
		} else {
			fmt.Printf(" Connection failed: %v\n", err)
		}
	}

	// Ask if user wants to save
	fmt.Print("\nSave this host configuration? (Y/n): ")
	saveStr, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read save preference: %w", err)
	}
	saveStr = strings.ToLower(strings.TrimSpace(saveStr))

	if saveStr == "" || saveStr == "y" || saveStr == "yes" {
		if err := saveDetectedNode(address, port, useHTTPS); err != nil {
			fmt.Printf(" Warning: Could not save host configuration: %v\n", err)
		} else {
			fmt.Println("Node configuration saved")
		}
	}

	return &ClientNodeConfig{
		Name:    address,
		Address: fmt.Sprintf("%s://%s:%d", scheme, address, port),
	}, nil
}
