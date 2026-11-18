package localproof

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalRecoveryProofLifecycle(t *testing.T) {
	dir := t.TempDir()
	descriptor := File{Endpoint: "http://127.0.0.1:9090", ConsoleURL: "http://localhost:9090/console/"}
	authority, err := New(t.Context(), dir, descriptor)
	require.NoError(t, err)
	file, err := Read(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, descriptor.Endpoint, file.Endpoint)
	require.True(t, authority.Verify(file.Proof))
	require.False(t, authority.Verify(strings.Repeat("x", 43)))
	require.False(t, authority.Verify(""))
	require.NotContains(t, authority.Reference(), file.Proof)
	if runtime.GOOS != "windows" {
		for path, mode := range map[string]os.FileMode{Path(dir): 0o600, filepath.Dir(Path(dir)): 0o700} {
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, mode, info.Mode().Perm())
		}
	}
	replacement, err := New(t.Context(), dir, descriptor)
	require.NoError(t, err)
	require.False(t, replacement.ValidReference(authority.Reference()))
	require.NoError(t, authority.Close(), "old owner must preserve a newer daemon's proof")
	file, err = Read(t.Context(), dir)
	require.NoError(t, err)
	require.True(t, replacement.Verify(file.Proof))
	require.NoError(t, replacement.Close())
	_, err = Read(t.Context(), dir)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, replacement.Close())
}

func TestLocalRecoveryRejectsInvalidFilesAndCanceledWork(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field": `{"version":1,"proof":"` + strings.Repeat("x", 43) + `","extra":true}`,
		"wrong version": `{"version":2,"proof":"` + strings.Repeat("x", 43) + `"}`,
		"short proof":   `{"version":1,"proof":"short"}`,
		"trailing data": `{"version":1,"proof":"` + strings.Repeat("x", 43) + `"} {}`,
		"oversized":     strings.Repeat(" ", 8193),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := Path(dir)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
			_, err := Read(t.Context(), dir)
			require.Error(t, err)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir := t.TempDir()
	_, err := New(ctx, dir, File{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(Path(dir))
	require.True(t, errors.Is(err, os.ErrNotExist))
	_, err = Read(ctx, dir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalRecoveryAddressRefusesDNSAndRemoteHosts(t *testing.T) {
	for _, host := range []string{"localhost", "router.example", "203.0.113.9", "0.0.0.0", "::", ""} {
		require.False(t, Address(host), host)
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		require.True(t, Address(host), host)
	}
}
