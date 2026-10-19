package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stretchr/testify/require"
)

func TestStrataTemplateArchivesExternalAtStartup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("historical recovery requires a nonroot node identity")
	}
	root := t.TempDir()
	old := filepath.Join(root, "external", "strata", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(old), 0700))
	body := []byte("protocol: openai\nenabled: false\nruntime: {endpoint: 'http://127.0.0.1:8080'}\nservice: {manager: none}\n")
	require.NoError(t, os.WriteFile(old, body, 0600))
	receipt, err := json.Marshal(map[string]any{"format": 1, "files": map[string]any{"external/strata/config.yaml": [][]string{{"version_source"}}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".managed-ownership.json"), receipt, 0600))
	_, err = templates.PrepareProviders(root)
	require.NoError(t, err)
	require.NoFileExists(t, old)
	archived, err := os.ReadFile(old + ".rollback")
	require.NoError(t, err)
	require.Equal(t, body, archived)
	apps, err := LoadAppsConfig(root)
	require.NoError(t, err, "startup must not find Strata in two kinds")
	require.Nil(t, apps.GetExternal("strata"))
	require.NotNil(t, apps.GetOnDemand("strata"))
}
