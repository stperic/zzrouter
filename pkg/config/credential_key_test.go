package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stperic/common/atomicfile"
	"github.com/stperic/common/secrets"
	"github.com/stretchr/testify/require"
)

func TestAutomaticCredentialKey(t *testing.T) {
	dir := t.TempDir()
	key, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	loaded, err := secrets.LoadKeyFile(CredentialKeyPath(dir, CredentialsConfig{}))
	require.NoError(t, err)
	require.Equal(t, key, loaded)
	clear(loaded)
	// Interrupted setup may leave a key but no ciphertext. Never replace it.
	again, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{})
	require.NoError(t, err)
	require.Equal(t, key, again)
	clear(again)
}

func TestCredentialKeyPreservesExistingState(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "explicit"}[explicit], func(t *testing.T) {
			dir := t.TempDir()
			cfg := CredentialsConfig{}
			if explicit {
				cfg.KeyFile = "operator.key"
			}
			path := CredentialKeyPath(dir, cfg)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))
			_, err := PrepareCredentialKey(t.Context(), dir, cfg)
			require.Error(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "invalid", string(data))
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.enc"), []byte("existing ciphertext"), 0o600))
			_, err = PrepareCredentialKey(t.Context(), dir, cfg)
			require.Error(t, err)
			_, err = os.Stat(path)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCredentialKeyExplicitAndPermissions(t *testing.T) {
	dir := t.TempDir()
	_, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{KeyFile: "missing"})
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(dir, "private"))
	require.ErrorIs(t, err, os.ErrNotExist)
	key, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{})
	require.NoError(t, err)
	defer clear(key)
	for _, path := range []string{"private/credentials.key", CredentialKeyPath(dir, CredentialsConfig{})} {
		loaded, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{KeyFile: path})
		require.NoError(t, err)
		require.Equal(t, key, loaded)
		clear(loaded)
	}
	if runtime.GOOS != "windows" {
		path := CredentialKeyPath(dir, CredentialsConfig{})
		require.NoError(t, os.Chmod(path, 0o644))
		_, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{})
		require.Error(t, err, "never silently repair an exposed key")
	}
}

func TestConcurrentCredentialKeyPreparation(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan []byte, 12)
	errorsSeen := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			key, err := PrepareCredentialKey(t.Context(), dir, CredentialsConfig{})
			if err != nil {
				errorsSeen <- err
			} else {
				results <- key
			}
		})
	}
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		require.True(t, errors.Is(err, atomicfile.ErrLocked), "%v", err)
	}
	loaded, err := secrets.LoadKeyFile(CredentialKeyPath(dir, CredentialsConfig{}))
	require.NoError(t, err)
	defer clear(loaded)
	for key := range results {
		require.True(t, bytes.Equal(loaded, key))
		clear(key)
	}
}
