package sqlitedb_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"
)

// TestDriverVersionPair reads the selected versions from go.mod, which lists
// every module the build uses: a Go 1.26 test binary's build info lists none.
func TestDriverVersionPair(t *testing.T) {
	data, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	mod, err := modfile.Parse("go.mod", data, nil)
	require.NoError(t, err)
	versions := make(map[string]string)
	for _, req := range mod.Require {
		versions[req.Mod.Path] = req.Mod.Version
	}
	for _, rep := range mod.Replace {
		versions[rep.Old.Path] = rep.New.Version
	}
	for path, want := range map[string]string{
		"modernc.org/sqlite": "v1.62.0",
		"modernc.org/libc":   "v1.80.1",
	} {
		require.Equal(t, want, versions[path], "%s: when upgrading the driver, set libc to the exact version in the driver's own go.mod and update this pair", path)
	}
}
