package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// ClusterConfig Tests
// =============================================================================

func TestClusterConfig_IsMaster(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mode     ClusterMode
		expected bool
	}{
		{"master mode", ClusterModeCoordinator, true},
		{"worker mode", ClusterModeWorker, false},
		{"disabled mode", ClusterModeDisabled, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := &ClusterConfig{Mode: tt.mode}
			assert.Equal(t, tt.expected, cc.IsMaster())
		})
	}
}

func TestClusterConfig_IsWorker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mode     ClusterMode
		expected bool
	}{
		{"master mode", ClusterModeCoordinator, false},
		{"worker mode", ClusterModeWorker, true},
		{"disabled mode", ClusterModeDisabled, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := &ClusterConfig{Mode: tt.mode}
			assert.Equal(t, tt.expected, cc.IsWorker())
		})
	}
}

func TestClusterConfig_IsDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mode     ClusterMode
		expected bool
	}{
		{"master mode", ClusterModeCoordinator, false},
		{"worker mode", ClusterModeWorker, false},
		{"disabled mode", ClusterModeDisabled, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cc := &ClusterConfig{Mode: tt.mode}
			assert.Equal(t, tt.expected, cc.IsDisabled())
		})
	}
}

func TestClusterConfig_SetMode(t *testing.T) {
	cc := &ClusterConfig{}

	cc.SetMode(ClusterModeCoordinator)
	assert.Equal(t, ClusterModeCoordinator, cc.Mode)

	cc.SetMode(ClusterModeWorker)
	assert.Equal(t, ClusterModeWorker, cc.Mode)

	cc.SetMode(ClusterModeDisabled)
	assert.Equal(t, ClusterModeDisabled, cc.Mode)
}

func TestClusterConfig_ToClusterConfig(t *testing.T) {
	cc := &ClusterConfig{
		Mode:     ClusterModeCoordinator,
		BindAddr: "0.0.0.0",
		BindPort: 8080,
	}

	result := cc.ToClusterConfig()
	assert.NotNil(t, result)
}

// =============================================================================
// ObservabilityConfig Tests
// =============================================================================

func TestObservabilityConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   ObservabilityConfig
		expected bool
	}{
		{
			"enabled true",
			ObservabilityConfig{Enabled: true},
			true,
		},
		{
			"enabled false",
			ObservabilityConfig{Enabled: false},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.config.IsEnabled())
		})
	}
}

func TestObservabilityConfig_IsTracingEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   ObservabilityConfig
		expected bool
	}{
		{
			"both enabled",
			ObservabilityConfig{Enabled: true, Tracing: TracingConfig{Enabled: true}},
			true,
		},
		{
			"only observability enabled",
			ObservabilityConfig{Enabled: true, Tracing: TracingConfig{Enabled: false}},
			false,
		},
		{
			"only tracing enabled",
			ObservabilityConfig{Enabled: false, Tracing: TracingConfig{Enabled: true}},
			false,
		},
		{
			"both disabled",
			ObservabilityConfig{Enabled: false, Tracing: TracingConfig{Enabled: false}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.config.IsTracingEnabled())
		})
	}
}

func TestObservabilityConfig_IsMetricsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   ObservabilityConfig
		expected bool
	}{
		{
			"both enabled",
			ObservabilityConfig{Enabled: true, Metrics: MetricsConfig{Enabled: true}},
			true,
		},
		{
			"only observability enabled",
			ObservabilityConfig{Enabled: true, Metrics: MetricsConfig{Enabled: false}},
			false,
		},
		{
			"only metrics enabled",
			ObservabilityConfig{Enabled: false, Metrics: MetricsConfig{Enabled: true}},
			false,
		},
		{
			"both disabled",
			ObservabilityConfig{Enabled: false, Metrics: MetricsConfig{Enabled: false}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.config.IsMetricsEnabled())
		})
	}
}

// Note: IsPrometheusEnabled tests removed - Prometheus field is in MetricsConfig.PrometheusPort

func TestObservabilityConfig_ToObservabilityConfig(t *testing.T) {
	config := &ObservabilityConfig{
		Enabled: true,
		Tracing: TracingConfig{Enabled: true},
		Metrics: MetricsConfig{Enabled: true},
	}

	result := config.ToObservabilityConfig()
	assert.NotNil(t, result)
}

// =============================================================================
// ClusterMode Tests
// =============================================================================

func TestClusterMode_Values(t *testing.T) {
	// Verify the values are distinct
	assert.NotEqual(t, ClusterModeDisabled, ClusterModeCoordinator)
	assert.NotEqual(t, ClusterModeDisabled, ClusterModeWorker)
	assert.NotEqual(t, ClusterModeCoordinator, ClusterModeWorker)
}

// =============================================================================
// NodeConfig Tests
// =============================================================================

func TestServeConfig_Fields(t *testing.T) {
	sc := ServeConfig{
		Bind: "localhost",
		Port: 8080,
		Name: "test-server",
	}

	assert.Equal(t, "localhost", sc.Bind)
	assert.Equal(t, 8080, sc.Port)
	assert.Equal(t, "test-server", sc.Name)
}

// =============================================================================
// TracingConfig Tests
// =============================================================================

func TestTracingConfig_Fields(t *testing.T) {
	tc := TracingConfig{
		Enabled:    true,
		SampleRate: 0.5,
	}

	assert.True(t, tc.Enabled)
	assert.Equal(t, 0.5, tc.SampleRate)
}

// =============================================================================
// MetricsConfig Tests
// =============================================================================

func TestMetricsConfig_Fields(t *testing.T) {
	mc := MetricsConfig{
		Enabled: true,
	}

	assert.True(t, mc.Enabled)
}

// =============================================================================
// AuthConfig Tests
// =============================================================================

func TestAuthConfig_Fields(t *testing.T) {
	hac := AuthConfig{
		AdminKey: "ADMIN_KEY",
		UserKey:  "USER_KEY",
	}

	assert.Equal(t, "ADMIN_KEY", hac.AdminKey)
	assert.Equal(t, "USER_KEY", hac.UserKey)
}

// =============================================================================
// NodeConfig Tests
// =============================================================================

func TestNodeConfig_Fields(t *testing.T) {
	hc := NodeConfig{
		Node: ServeConfig{
			Bind: "0.0.0.0",
			Port: 9080,
			Name: "zzrouter-node",
		},
		Cluster: ClusterConfig{
			Mode: ClusterModeCoordinator,
		},
		Auth: AuthConfig{
			AdminKey: "ZZROUTER_ADMIN_API_KEY",
		},
		Observability: ObservabilityConfig{
			Enabled: true,
		},
	}

	assert.Equal(t, "0.0.0.0", hc.Node.Bind)
	assert.Equal(t, 9080, hc.Node.Port)
	assert.True(t, hc.Cluster.IsMaster())
	assert.Equal(t, "ZZROUTER_ADMIN_API_KEY", hc.Auth.AdminKey)
	assert.True(t, hc.Observability.IsEnabled())
}

// =============================================================================
// ClientNodeConfig Tests
// =============================================================================

func TestClientNodeConfig_Fields(t *testing.T) {
	chc := ClientNodeConfig{
		Name:    "production",
		Address: "https://api.example.com:443",
		APIKey:  "test-api-key",
	}

	assert.Equal(t, "production", chc.Name)
	assert.Equal(t, "https://api.example.com:443", chc.Address)
	assert.Equal(t, "test-api-key", chc.APIKey)
}

// =============================================================================
// UpdateConfig Validation Tests
// =============================================================================

func TestValidateMaintenanceWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		window    string
		expectErr bool
	}{
		{"empty is valid", "", false},
		{"daily at 3 AM", "0 3 * * *", false},
		{"weekly Sunday at midnight", "0 0 * * 0", false},
		{"weekends at 3 AM", "0 3 * * 6,0", false},
		{"specific day and month", "0 3 15 1 *", false},
		{"minute 30", "30 14 * * *", false},
		{"too few fields", "0 3 * *", true},
		{"too many fields", "0 3 * * * *", true},
		{"invalid minute", "60 3 * * *", true},
		{"invalid hour", "0 25 * * *", true},
		{"invalid day of month", "0 3 32 * *", true},
		{"invalid month", "0 3 * 13 *", true},
		{"invalid day of week", "0 3 * * 7", true},
		{"negative minute", "-1 3 * * *", true},
		{"non-numeric field", "abc 3 * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateMaintenanceWindow(tt.window)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateVersionPin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pin       string
		expectErr bool
	}{
		{"empty is valid", "", false},
		{"exact version", "1.2.3", false},
		{"patch wildcard", "1.2.x", false},
		{"minor wildcard", "1.x", false},
		{"major only", "1", false},
		{"full wildcard", "x.x.x", false},
		{"uppercase X", "1.2.X", false},
		{"with spaces trimmed", "  1.2.3  ", false},
		{"too many parts", "1.2.3.4", true},
		{"negative version", "-1.0.0", true},
		{"invalid character", "1.a.3", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateVersionPin(tt.pin)
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestUpdateConfig_Validate(t *testing.T) {
	t.Parallel()

	t.Run("valid config", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			Channel:              "stable",
			CheckIntervalHours:   24,
			MaintenanceWindow:    "0 3 * * *",
			PinnedVersion:        "1.x",
			KeepPreviousVersions: 2,
		}
		assert.NoError(t, cfg.Validate())
	})

	t.Run("invalid channel", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			Channel: "invalid",
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
		// Field name stays asserted as operator-facing context.
		assert.Contains(t, err.Error(), "update channel")
	})

	t.Run("negative check interval", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			CheckIntervalHours: -1,
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
		assert.Contains(t, err.Error(), "check_interval_hours")
	})

	t.Run("invalid maintenance window", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			MaintenanceWindow: "invalid",
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
		assert.Contains(t, err.Error(), "maintenance_window")
	})

	t.Run("invalid pinned version", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			PinnedVersion: "not-a-version",
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
		assert.Contains(t, err.Error(), "pinned_version")
	})

	t.Run("negative keep previous versions", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{
			KeepPreviousVersions: -1,
		}
		err := cfg.Validate()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidConfig)
		assert.Contains(t, err.Error(), "keep_previous_versions")
	})

	t.Run("empty config is valid", func(t *testing.T) {
		t.Parallel()
		cfg := &UpdateConfig{}
		assert.NoError(t, cfg.Validate())
	})
}

// =============================================================================
// NodeConfig Validate Tests
// =============================================================================

func TestNodeConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  NodeConfig
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid minimal config",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}},
			wantErr: false,
		},
		{
			name:    "invalid port zero",
			config:  NodeConfig{Node: ServeConfig{Port: 0}},
			wantErr: true,
			errMsg:  "port",
		},
		{
			name:    "invalid port too high",
			config:  NodeConfig{Node: ServeConfig{Port: 70000}},
			wantErr: true,
			errMsg:  "port",
		},
		{
			name:    "admin key too short",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}, Auth: AuthConfig{AdminKey: "short"}},
			wantErr: true,
			errMsg:  "admin_key",
		},
		{
			name:    "admin key valid length",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}, Auth: AuthConfig{AdminKey: "this-is-a-valid-key-with-32-chars"}},
			wantErr: false,
		},
		{
			name:    "user key too short",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}, Auth: AuthConfig{UserKey: "short"}},
			wantErr: true,
			errMsg:  "user_key",
		},
		{
			name:    "cluster key not required when disabled",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: ClusterModeDisabled}},
			wantErr: false,
		},
		{
			// A worker learns its coordinator by pairing, so no
			// endpoint list is needed — and requiring one used to
			// hand operators a config that load-time promoted the
			// node straight back to coordinator.
			name:    "worker mode needs no endpoints",
			config:  NodeConfig{Node: ServeConfig{Port: 9090}, Cluster: ClusterConfig{Mode: ClusterModeWorker}},
			wantErr: false,
		},
		{
			name: "worker mode rejects endpoints",
			config: NodeConfig{
				Node:    ServeConfig{Port: 9090},
				Cluster: ClusterConfig{Mode: ClusterModeWorker, Endpoints: PeersFromAddresses("http://10.0.0.2:9090")},
			},
			wantErr: true,
			errMsg:  "endpoints",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrInvalidConfig)
				// errMsg retained as a field-name hint — the wrap must
				// surface the offending field so operators can act.
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
