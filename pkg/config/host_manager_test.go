package config

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Port Functions Tests
// =============================================================================

// =============================================================================
// ConfigManager Node Config Tests
// =============================================================================

func TestConfigManager_SaveAndLoadNodeConfig(t *testing.T) {
	// Create temp directory
	tempDir, err := os.MkdirTemp("", "host-config-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	cm := NewConfigManager("test-app")

	config := &NodeConfig{
		Node: ServeConfig{
			Bind: "0.0.0.0",
			Port: 8888,
			Name: "saved-server",
		},
		Cluster: ClusterConfig{
			Mode: ClusterModeDisabled,
		},
	}

	// Save config
	err = cm.SaveNodeConfigToDir(config, tempDir)
	require.NoError(t, err)

	// Verify file was created
	serverPath := filepath.Join(tempDir, "node.yaml")
	_, err = os.Stat(serverPath)
	assert.NoError(t, err, "node.yaml should exist")

	// Load config back
	loadedConfig, err := cm.LoadNodeConfigFromDir(tempDir)
	require.NoError(t, err)
	require.NotNil(t, loadedConfig)

	assert.Equal(t, config.Node.Bind, loadedConfig.Node.Bind)
	assert.Equal(t, config.Node.Port, loadedConfig.Node.Port)
}

func TestConfigManager_LoadNodeConfig_ValidFile(t *testing.T) {
	// Create temp directory with valid config
	tempDir, err := os.MkdirTemp("", "host-config-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Create valid node.yaml
	serverPath := filepath.Join(tempDir, "node.yaml")
	configContent := `
node:
  bind: localhost
  port: 9080
  name: test-server
cluster:
  mode: disabled
auth:
  admin_key: test-admin-key-32chars-minimum!
`
	// 0o600: requireSafePerms refuses world-readable config files now.
	// Loaders that previously tolerated 0o644 must use tight perms in
	// fixtures (this is the same posture every production deployment
	// must hold).
	require.NoError(t, os.WriteFile(serverPath, []byte(configContent), 0o600))

	cm := NewConfigManager("test-app")

	config, err := cm.LoadNodeConfigFromDir(tempDir)
	require.NoError(t, err)
	require.NotNil(t, config)

	assert.Equal(t, "localhost", config.Node.Bind)
	assert.Equal(t, 9080, config.Node.Port)
	assert.Equal(t, "test-server", config.Node.Name)
}

// A fresh node answers keyless inference only where nobody else can reach
// it, so a template that binds every interface must require a key.
func TestNodeTemplate_KeylessInferenceOnlyOnLoopback(t *testing.T) {
	cfg, err := createNodeConfigFile(filepath.Join(t.TempDir(), "node.yaml"))
	require.NoError(t, err)
	ip := net.ParseIP(cfg.Node.Bind)
	loopback := cfg.Node.Bind == "localhost" || (ip != nil && ip.IsLoopback())
	assert.Equal(t, !loopback, cfg.Auth.RequireCompatAuth,
		"bind %q: require_compat_auth must be true unless the node is loopback-only", cfg.Node.Bind)
}

// security.require_auth was removed: no code ever read it. Every node.yaml
// written from the old template still carries it, and must still load.
func TestConfigManager_LoadNodeConfig_IgnoresRetiredRequireAuth(t *testing.T) {
	dir := t.TempDir()
	content := `
node:
  bind: localhost
  port: 9080
  name: test-server
cluster:
  mode: disabled
security:
  log_auth_attempts: true
  require_auth: true
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(content), 0o600))

	config, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.True(t, config.Security.LogAuthAttempts)
}

func TestConfigManager_LoadNodeConfig_InvalidYAML(t *testing.T) {
	// Create temp directory with invalid config
	tempDir, err := os.MkdirTemp("", "host-config-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Create invalid YAML
	serverPath := filepath.Join(tempDir, "node.yaml")
	invalidContent := `
server:
  host: localhost
  port: [invalid yaml
`
	require.NoError(t, os.WriteFile(serverPath, []byte(invalidContent), 0644))

	cm := NewConfigManager("test-app")

	config, err := cm.LoadNodeConfigFromDir(tempDir)
	assert.Error(t, err)
	assert.Nil(t, config)
}

func TestConfigManager_LoadNodeConfig_EmptyFile(t *testing.T) {
	// Create temp directory with empty config file
	tempDir, err := os.MkdirTemp("", "host-config-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Create empty node.yaml
	serverPath := filepath.Join(tempDir, "node.yaml")
	require.NoError(t, os.WriteFile(serverPath, []byte(""), 0644))

	cm := NewConfigManager("test-app")

	// Loading empty file should work (defaults will be used)
	config, err := cm.LoadNodeConfigFromDir(tempDir)
	// Either works with defaults or returns error - both acceptable
	if err == nil {
		require.NotNil(t, config)
	}
}

// =============================================================================
// ConfigManager Creation Tests
// =============================================================================

func TestNewConfigManager_NodeManager(t *testing.T) {
	cm := NewConfigManager("test-app")
	assert.NotNil(t, cm)
}

func TestNewConfigManager_EmptyNameNodeManager(t *testing.T) {
	cm := NewConfigManager("")
	assert.NotNil(t, cm)
}

// =============================================================================
// SaveNodeConfigToDir Roundtrip Tests
// =============================================================================

func TestConfigManager_SaveNodeConfig_Roundtrip_WithEndpoints(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "host-config-roundtrip")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	cm := NewConfigManager("test-app")

	original := &NodeConfig{
		Node: ServeConfig{
			Name: "coordinator-1",
			Bind: "0.0.0.0",
			Port: 9090,
		},
		Cluster: ClusterConfig{
			Mode: ClusterModeCoordinator,
			Endpoints: PeersFromAddresses(
				"192.0.2.20:9090",
				"192.0.2.21:9090",
			),
		},
		Auth: AuthConfig{
			AdminKey: "admin-key-32chars-minimum-value!",
			UserKey:  "user-key-32chars-minimum-value!!",
		},
		MDNSDiscovery: true,
	}

	// Save
	err = cm.SaveNodeConfigToDir(original, tempDir)
	require.NoError(t, err)

	// Verify file exists and is valid YAML
	configPath := filepath.Join(tempDir, "node.yaml")
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Load back via Viper (same path as production)
	loaded, err := cm.LoadNodeConfigFromDir(tempDir)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	// Verify all sections roundtrip correctly
	assert.Equal(t, original.Node.Name, loaded.Node.Name)
	assert.Equal(t, original.Node.Bind, loaded.Node.Bind)
	assert.Equal(t, original.Node.Port, loaded.Node.Port)
	assert.Equal(t, original.Cluster.Mode, loaded.Cluster.Mode)
	assert.Equal(t, original.Cluster.Endpoints, loaded.Cluster.Endpoints)
	assert.Equal(t, original.Auth.AdminKey, loaded.Auth.AdminKey)
	assert.Equal(t, original.Auth.UserKey, loaded.Auth.UserKey)
	assert.Equal(t, original.MDNSDiscovery, loaded.MDNSDiscovery)
}

func TestConfigManager_SaveNodeConfig_Roundtrip_NoEndpoints(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "host-config-roundtrip")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	cm := NewConfigManager("test-app")

	original := &NodeConfig{
		Node: ServeConfig{
			Name: "worker-1",
			Bind: "0.0.0.0",
			Port: 9090,
		},
		Cluster: ClusterConfig{
			Mode: ClusterModeWorker,
		},
	}

	err = cm.SaveNodeConfigToDir(original, tempDir)
	require.NoError(t, err)

	loaded, err := cm.LoadNodeConfigFromDir(tempDir)
	require.NoError(t, err)

	assert.Equal(t, original.Node.Name, loaded.Node.Name)
	assert.Equal(t, original.Cluster.Mode, loaded.Cluster.Mode)
	assert.Empty(t, loaded.Cluster.Endpoints)
}

func TestConfigManager_SaveNodeConfig_OverwritePreservesOwnership(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "host-config-ownership")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tempDir) }()

	cm := NewConfigManager("test-app")

	config := &NodeConfig{
		Node: ServeConfig{
			Name: "test-node",
			Bind: "0.0.0.0",
			Port: 9090,
		},
	}

	// Write twice — second write should succeed (exercises the ownership path)
	err = cm.SaveNodeConfigToDir(config, tempDir)
	require.NoError(t, err)

	config.Node.Name = "updated-node"
	err = cm.SaveNodeConfigToDir(config, tempDir)
	require.NoError(t, err)

	loaded, err := cm.LoadNodeConfigFromDir(tempDir)
	require.NoError(t, err)
	assert.Equal(t, "updated-node", loaded.Node.Name)
}

// =============================================================================
// Load-time normalization
// =============================================================================

// writeNodeConfig writes body as dir/node.yaml with the perms the loader
// insists on.
func writeNodeConfig(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(body), 0600))
}

// TestLoadNodeConfig_KeepsWorkerModeWithEndpoints is the regression that
// cost the most on a real onboarding: load used to promote any node
// carrying cluster.endpoints to coordinator, silently overriding an
// explicit `mode: worker`. The only symptom was `cluster pair` refusing
// with "this node is a coordinator", which names neither the field nor
// the override.
func TestLoadNodeConfig_KeepsWorkerModeWithEndpoints(t *testing.T) {
	dir := t.TempDir()
	writeNodeConfig(t, dir, `
node:
  name: worker-1
  bind: 0.0.0.0
  port: 9090
cluster:
  mode: worker
  endpoints:
    - http://10.0.0.2:9090
`)

	cfg, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.Equal(t, ClusterModeWorker, cfg.Cluster.Mode)
}

// TestLoadNodeConfig_KeepsCoordinatorMode pins the other half: a
// coordinator with endpoints is left alone too, so the deleted
// auto-promotion is not silently reintroduced as "harmless".
func TestLoadNodeConfig_KeepsCoordinatorMode(t *testing.T) {
	dir := t.TempDir()
	writeNodeConfig(t, dir, `
node:
  name: coord-1
  bind: 0.0.0.0
  port: 9090
cluster:
  mode: coordinator
  endpoints:
    - http://10.0.0.2:9090
`)

	cfg, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.Equal(t, ClusterModeCoordinator, cfg.Cluster.Mode)
}

// TestLoadNodeConfig_DefaultsNameToHostname covers the "defaults to
// hostname" the template promises. It used to be applied by the CLI
// only, so a node started as a service kept an empty name and the
// cluster listed it by URL.
func TestLoadNodeConfig_DefaultsNameToHostname(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)
	require.NotEmpty(t, hostname)

	dir := t.TempDir()
	writeNodeConfig(t, dir, `
node:
  name: ""
  bind: 0.0.0.0
  port: 9090
cluster:
  mode: worker
`)

	cfg, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.Equal(t, hostname, cfg.Node.Name)
}

// TestLoadNodeConfig_KeepsConfiguredName confirms the default only
// fills a zero value.
func TestLoadNodeConfig_KeepsConfiguredName(t *testing.T) {
	dir := t.TempDir()
	writeNodeConfig(t, dir, `
node:
  name: chosen-name
  bind: 0.0.0.0
  port: 9090
`)

	cfg, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "chosen-name", cfg.Node.Name)
}

// TestLoadNodeConfig_FreshTemplateGetsDefaults covers the path a brand
// new node takes: the template ships `name: ""`, and the node must
// still come up with an identity rather than resolving one later, or
// not at all.
func TestLoadNodeConfig_FreshTemplateGetsDefaults(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	dir := t.TempDir()
	cfg, err := NewConfigManager("test-app").LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(dir, "node.yaml"))
	assert.Equal(t, hostname, cfg.Node.Name)
}

// Exercise the Viper loader, whose mapstructure tags differ from YAML tags.
func TestNodeConfig_MCPAuthenticationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	content := `
node:
  bind: localhost
  port: 9080
  name: test-mcp
cluster:
  mode: disabled
mcp:
  servers:
    token:
      url: https://mcp.example.test
      api:
        auth_type: api-key
        auth_header: X-Api-Key
        auth_prefix: "Token "
        token: test-token
        custom_headers:
          X-Version: v1
        rate_limit:
          requests_per_minute: 12
          tokens_per_minute: 34
    caller:
      url: https://mcp.example.test
      api:
        auth_type: caller
    custom:
      url: https://mcp.example.test
      api:
        auth_type: custom
        custom_headers:
          X-Credential: test-credential
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(content), 0o600))
	cm := NewConfigManager("test-app")
	cfg, err := cm.LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	api := cfg.MCP.Servers["token"].API
	require.NotNil(t, api)
	assert.Equal(t, AuthTypeAPIKey, api.AuthType)
	h := http.Header{}
	api.ApplyAuthHeaders(h)
	assert.Equal(t, "Token test-token", h.Get("X-Api-Key"))
	assert.Empty(t, h.Get("Authorization"))
	assert.Equal(t, "v1", h.Get("X-Version"))
	require.NotNil(t, api.RateLimit)
	assert.Equal(t, 12, api.RateLimit.RequestsPerMinute)
	assert.Equal(t, 34, api.RateLimit.TokensPerMinute)
	assert.Equal(t, AuthTypeCaller, cfg.MCP.Servers["caller"].API.AuthType)
	assert.Equal(t, AuthTypeCustom, cfg.MCP.Servers["custom"].API.AuthType)
	assert.Equal(t, "test-credential", cfg.MCP.Servers["custom"].API.CustomHeaders["x-credential"])
	require.NoError(t, cm.SaveNodeConfigToDir(cfg, dir))
	reloaded, err := cm.LoadNodeConfigFromDir(dir)
	require.NoError(t, err)
	assert.Equal(t, cfg.MCP, reloaded.MCP)
}
