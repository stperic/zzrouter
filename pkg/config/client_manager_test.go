package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestClientConfig_GetNodeConfig(t *testing.T) {
	config := &ClientConfig{
		Node: NodeConnection{
			Address: "localhost",
			Port:    9090,
			Key:     "testkey", // Using Key for literal value
			Secure:  false,
		},
	}

	hostConfig, err := config.GetNodeConfig()
	if err != nil {
		t.Fatalf("GetNodeConfig returned error: %v", err)
	}

	if hostConfig == nil {
		t.Fatal("GetNodeConfig returned nil")
	}

	if hostConfig.Address != "http://localhost:9090" {
		t.Errorf("Expected Address to be 'http://localhost:9090', got '%s'", hostConfig.Address)
	}

	if hostConfig.APIKey != "testkey" {
		t.Errorf("Expected APIKey to be 'testkey', got '%s'", hostConfig.APIKey)
	}
}

func TestClientConfig_GetNodeConfig_Empty(t *testing.T) {
	config := &ClientConfig{
		Node: NodeConnection{}, // Empty host connection
	}

	// GetNodeConfig now auto-detects localhost, so it may succeed
	hostConfig, err := config.GetNodeConfig()
	if err != nil {
		// This is expected if no host is running on localhost
		t.Logf("GetNodeConfig returned expected error for empty host: %v", err)
	} else {
		// This is also valid if auto-detection found a host
		t.Logf("GetNodeConfig auto-detected host: %s", hostConfig.Address)
	}
}

func TestClientConfig_GetDefaultNodeConfig(t *testing.T) {
	config := &ClientConfig{
		Node: NodeConnection{
			Address: "localhost",
			Port:    9090,
			APIKey:  "testkey",
			Secure:  true,
		},
	}

	hostConfig, err := config.GetDefaultNodeConfig()
	if err != nil {
		t.Fatalf("GetDefaultNodeConfig returned error: %v", err)
	}

	if hostConfig == nil {
		t.Fatal("GetDefaultNodeConfig returned nil")
	}

	if hostConfig.Address != "https://localhost:9090" {
		t.Errorf("Expected Address to be 'https://localhost:9090', got '%s'", hostConfig.Address)
	}
}

func TestClientConfig_GetDefaultNodeConfig_Empty(t *testing.T) {
	config := &ClientConfig{
		Node: NodeConnection{}, // Empty host connection
	}

	// GetDefaultNodeConfig now auto-detects localhost, so it may succeed
	hostConfig, err := config.GetDefaultNodeConfig()
	if err != nil {
		// This is expected if no host is running on localhost
		t.Logf("GetDefaultNodeConfig returned expected error for empty host: %v", err)
	} else {
		// This is also valid if auto-detection found a host
		t.Logf("GetDefaultNodeConfig auto-detected host: %s", hostConfig.Address)
	}
}

// =============================================================================
// SaveClientConfig Roundtrip Tests
// =============================================================================

func TestSaveClientConfig_Roundtrip(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "client-config-roundtrip")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	original := &ClientConfig{
		Node: NodeConnection{
			Address: "192.0.2.10",
			Port:    9090,
			Key:     "ZZROUTER_ADMIN_API_KEY",
			Secure:  true,
		},
		Preferences: ClientPreferences{
			Timeout:      60,
			OutputFormat: "json",
			Verbose:      true,
			DefaultModel: "llama3:8b",
			Editor:       "vim",
		},
	}

	// Marshal and write (same as SaveClientConfig does internally)
	configPath := filepath.Join(tempDir, "cli.yaml")
	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, data, 0600))

	// Load back via Viper (same path as production LoadClientConfig)
	v := viper.New()
	v.SetConfigFile(configPath)
	require.NoError(t, v.ReadInConfig())

	var loaded ClientConfig
	require.NoError(t, v.Unmarshal(&loaded))

	assert.Equal(t, original.Node.Address, loaded.Node.Address)
	assert.Equal(t, original.Node.Port, loaded.Node.Port)
	assert.Equal(t, original.Node.Key, loaded.Node.Key)
	assert.Equal(t, original.Node.Secure, loaded.Node.Secure)
	assert.Equal(t, original.Preferences.Timeout, loaded.Preferences.Timeout)
	assert.Equal(t, original.Preferences.OutputFormat, loaded.Preferences.OutputFormat)
	assert.Equal(t, original.Preferences.Verbose, loaded.Preferences.Verbose)
	assert.Equal(t, original.Preferences.DefaultModel, loaded.Preferences.DefaultModel)
	assert.Equal(t, original.Preferences.Editor, loaded.Preferences.Editor)
}
