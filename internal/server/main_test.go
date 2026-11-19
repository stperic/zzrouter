package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/access/sqlstore"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
)

// TestMain gives the whole package a throwaway config home. A server built
// straight from NewServerWithOptions opens the access store in the config
// dir, so without one a test run reads, and can write, the developer's real
// one. BuildTestServer still sets its own home per server, and seeds each
// coordinator's with a copy of one template store made here.
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
	if testStoreTemplate, err = os.MkdirTemp("", "zzrouter-server-store-"); err == nil {
		var store *sqlstore.Store
		if store, err = sqlstore.Open(context.Background(), testStoreTemplate, sqlstore.Options{}); err == nil {
			err = store.Close()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the template access store:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	_ = os.RemoveAll(testStoreTemplate)
	os.Exit(code)
}

func TestPackageConfigDirIsTheTestHome(t *testing.T) {
	home := os.Getenv("ZZROUTER_TEST_HOME")
	dir := pkgConfig.NewConfigManager("zzrouter").GetNodeConfigDir()
	if home == "" || !strings.HasPrefix(dir, home) {
		t.Fatalf("config dir %q is outside the test home %q", dir, home)
	}
}
