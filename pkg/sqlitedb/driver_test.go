package sqlitedb_test

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDriverVersionPair(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok, "test binary must contain module build information")
	versions := make(map[string]string)
	for _, dep := range info.Deps {
		version := dep.Version
		if dep.Replace != nil {
			version = dep.Replace.Version
		}
		versions[dep.Path] = version
	}
	for path, want := range map[string]string{
		"modernc.org/sqlite": "v1.62.0",
		"modernc.org/libc":   "v1.80.1",
	} {
		require.Equal(t, want, versions[path], "%s: when upgrading the driver, set libc to the exact version in the driver's own go.mod and update this pair", path)
	}
}
