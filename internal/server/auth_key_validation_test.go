package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestNewServer_RejectsWeakKey asserts NewServerWithOptions runs
// ValidateAPIKey at boot so weak/short keys fail loudly instead of
// being silently accepted at request time.
func TestNewServer_RejectsWeakKey(t *testing.T) {
	cases := []struct {
		name       string
		adminKey   string
		wantErr    string
		wantAccept bool
	}{
		{
			name:       "valid strong key accepted",
			adminKey:   TestAdminKey,
			wantAccept: true,
		},
		{
			name:     "too short",
			adminKey: "mx-only-8",
			wantErr:  "too short",
		},
		{
			name:     "weak pattern (password)",
			adminKey: "password-padding-to-reach-32-chars",
			wantErr:  "weak pattern",
		},
		{
			name:     "weak pattern (admin)",
			adminKey: "mx-administrator-32chars-minimum0",
			wantErr:  "weak pattern",
		},
		{
			name:     "low entropy (single char)",
			adminKey: strings.Repeat("a", 40),
			wantErr:  "entropy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &pkgConfig.NodeConfig{
				Node: pkgConfig.ServeConfig{
					Bind: "localhost",
					Port: getNextTestPort(),
					Name: "test-host",
				},
				Cluster: pkgConfig.ClusterConfig{Mode: pkgConfig.ClusterModeDisabled},
				Auth:    pkgConfig.AuthConfig{AdminKey: tc.adminKey},
			}

			server, err := NewServerWithOptions(cfg)
			if server != nil {
				t.Cleanup(func() { cleanupTestNode(server) })
			}

			if tc.wantAccept {
				require.NoError(t, err, "expected valid key to be accepted")
				return
			}

			require.Error(t, err, "expected weak key to be rejected at boot")
			assert.Contains(t, err.Error(), tc.wantErr,
				"error message should mention why the key was rejected")
		})
	}
}
