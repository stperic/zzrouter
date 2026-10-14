package update

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
)

// TestMain gives the whole package a throwaway config home. A scheduler built
// with NewScheduler records history and its operation in the config dir, so
// without one a test run writes failed fake updates into the developer's real
// update state. Tests that need their own home still set it per test.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zzrouter-update-test-")
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
	dir := config.NewConfigManager("zzrouter").GetNodeConfigDir()
	if home == "" || !strings.HasPrefix(dir, home) {
		t.Fatalf("config dir %q is outside the test home %q", dir, home)
	}
}
