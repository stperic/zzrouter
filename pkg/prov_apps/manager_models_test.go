package prov_apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
)

// TestMain isolates model fixtures and every package-owned runtime path from
// the operator's configuration, PID records and managed provider installations.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "zzrouter-prov-apps")
	if err != nil {
		panic("create test models root: " + err.Error())
	}
	modelsRoot := filepath.Join(root, "models")
	for _, model := range []string{"llama3", "llama3-seq", "llama3-dedupe", "a", "b"} {
		if err := os.MkdirAll(filepath.Join(modelsRoot, model), 0o755); err != nil {
			panic("seed test model: " + err.Error())
		}
	}
	if err := os.Setenv("ZZROUTER_TEST_HOME", filepath.Join(root, "state")); err != nil {
		panic("isolate test runtime paths: " + err.Error())
	}
	fsroot.SetProviderRootOverride(filepath.Join(root, "providers"))
	modelregistry.SetModelsRootDirOverride(modelsRoot)

	code := m.Run()

	modelregistry.ClearModelsRootDirOverride()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
