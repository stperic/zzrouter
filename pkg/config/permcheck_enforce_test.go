package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireSafePerms must accept a 0600 file (owner-only).
func TestRequireSafePerms_AcceptsTightPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm-bit semantics are POSIX; Windows uses ACL path covered separately")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

	require.NoError(t, requireSafePerms(path))
}

// World-readable file (0o644) must produce ErrFilePermsTooOpen unless
// the escape hatch env var is set.
func TestRequireSafePerms_RefusesWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm-bit semantics are POSIX; Windows uses ACL path covered separately")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	err := requireSafePerms(path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFilePermsTooOpen))
	assert.Contains(t, err.Error(), "ZZROUTER_ALLOW_INSECURE_PERMS",
		"error message must point operator at the override")
}

// Escape hatch must allow a world-readable file through. Use t.Setenv so
// the var is restored after the test (no test-pollution).
func TestRequireSafePerms_EnvOverrideAllowsWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm-bit semantics are POSIX")
	}
	// NOT t.Parallel — t.Setenv panics inside parallel subtests.
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	t.Setenv("ZZROUTER_ALLOW_INSECURE_PERMS", "1")
	require.NoError(t, requireSafePerms(path))
}

// A non-existent file must NOT be treated as a perm error — caller
// will surface the real read error from the loader. The gate's job
// is "block loading a too-open file"; "file missing" is a different
// concern.
func TestRequireSafePerms_NoFileIsNotAPermError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, requireSafePerms(filepath.Join(dir, "missing.yaml")))
}

// Only the literal string "1" enables the override. "true", "yes",
// "0", or empty string must NOT bypass the check — operators should
// have to opt in deliberately.
func TestRequireSafePerms_OverrideRequiresLiteralOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX perm-bit semantics")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yaml")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	for _, val := range []string{"", "0", "true", "yes", "TRUE", " 1 ", "1\n"} {
		t.Run("val_"+val, func(t *testing.T) {
			t.Setenv("ZZROUTER_ALLOW_INSECURE_PERMS", val)
			err := requireSafePerms(path)
			require.Error(t, err, "value %q must not bypass the gate", val)
		})
	}
}
