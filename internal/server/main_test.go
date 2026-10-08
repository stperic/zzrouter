package server

import (
	"fmt"
	"os"
	"strings"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestMain gives the whole package a throwaway config home. A server built
// straight from NewServerWithOptions opens keys.yaml and teams.yaml in the
// config dir and saves them back on Stop, so without one a test run reads,
// and can overwrite, the developer's real ones. BuildTestServer still sets
// its own home per server.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zzrouter-server-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test home:", err)
		os.Exit(1)
	}
	if err := os.Setenv("ZZROUTER_TEST_HOME", home); err != nil { // lint:allow os.Setenv
		fmt.Fprintln(os.Stderr, "set ZZROUTER_TEST_HOME:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestPackageConfigDirIsTheTestHome(t *testing.T) {
	home := os.Getenv("ZZROUTER_TEST_HOME")
	dir := pkgConfig.NewConfigManager("zzrouter").GetNodeConfigDir()
	if home == "" || !strings.HasPrefix(dir, home) {
		t.Fatalf("config dir %q is outside the test home %q", dir, home)
	}
}
