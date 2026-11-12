package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerFixturesIsolateProviderInstalls(t *testing.T) {
	previousRoot := fsroot.ProviderRootDir()
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })
	ambientRoot := t.TempDir()
	fsroot.SetProviderRootOverride(ambientRoot)
	versionFile := fsroot.ProviderVersionFile("llamacpp")
	require.NoError(t, os.MkdirAll(filepath.Dir(versionFile), 0o700))
	require.NoError(t, os.WriteFile(versionFile, []byte("fixture-existing-install"), 0o600))

	for _, kind := range []string{"seeded", "builder"} {
		t.Run(kind, func(t *testing.T) {
			var s *Server
			if kind == "seeded" {
				s = createSeededTestNode(t)
			} else {
				cfg := DefaultTestNodeConfig()
				cfg.SeedProvidersDir = true
				var cleanup func()
				var err error
				s, cleanup, err = BuildTestServer(cfg)
				require.NoError(t, err)
				t.Cleanup(cleanup)
			}
			isolatedRoot := fsroot.ProviderRootDir()
			require.Equal(t, filepath.Join(os.Getenv("ZZROUTER_TEST_HOME"), "installed-providers"), isolatedRoot)
			require.NotEqual(t, ambientRoot, isolatedRoot)
			info := fixtureProviderInfo(t, s, "llamacpp")
			assert.False(t, info.Managed)
			assert.False(t, info.State.IsInstalled())

			// Reproduce the old fixture leak using only a synthetic install. A
			// managed install wins over an empty version cache by design; the
			// harness must isolate that evidence, not change provider semantics.
			fsroot.SetProviderRootOverride(ambientRoot)
			s.providers.appMgr.SetProviderVersion("llamacpp", "")
			info = fixtureProviderInfo(t, s, "llamacpp")
			assert.True(t, info.Managed)
			assert.True(t, info.State.IsInstalled())
			fsroot.SetProviderRootOverride(isolatedRoot)
			info = fixtureProviderInfo(t, s, "llamacpp")
			assert.False(t, info.Managed)
			assert.False(t, info.State.IsInstalled())
		})
		assert.Equal(t, ambientRoot, fsroot.ProviderRootDir(), "cleanup restores the enclosing fixture root")
	}
	version, err := os.ReadFile(versionFile)
	require.NoError(t, err)
	assert.Equal(t, "fixture-existing-install", string(version))
}

func fixtureProviderInfo(t *testing.T, s *Server, name string) prov_apps.LocalProviderInfo {
	t.Helper()
	for _, info := range s.providers.appMgr.LocalInfo("") {
		if info.Key == name {
			return info
		}
	}
	t.Fatalf("provider %q missing from fixture", name)
	return prov_apps.LocalProviderInfo{}
}
