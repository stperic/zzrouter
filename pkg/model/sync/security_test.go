package sync

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurity_SiblingPrefixRejected asserts that a sibling dir sharing
// a string prefix with modelsRoot ("<root>-evil") is NOT considered
// inside it — HasPrefix got this wrong.
func TestSecurity_SiblingPrefixRejected(t *testing.T) {
	base := t.TempDir()

	modelsRoot := filepath.Join(base, "models")
	require.NoError(t, os.MkdirAll(modelsRoot, 0o755))

	siblingDir := filepath.Join(base, "models-evil")
	require.NoError(t, os.MkdirAll(siblingDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(siblingDir, "secret"), []byte("loot"), 0o600))

	s := NewSecurityFromRoot(modelsRoot)

	assert.False(t, isWithin(modelsRoot, siblingDir))
	assert.False(t, isWithin(modelsRoot, filepath.Join(siblingDir, "secret")))

	legit := filepath.Join(modelsRoot, "allowed.gguf")
	require.NoError(t, os.WriteFile(legit, []byte("ok"), 0o600))
	assert.True(t, isWithin(modelsRoot, legit))

	_, err := s.ValidateModelPath("allowed.gguf")
	assert.NoError(t, err)
}

// TestSecurity_SymlinkEscapeRejected asserts a symlink inside modelsRoot
// pointing to an outside target is rejected — os.Stat would follow the
// link, so ValidateModelPath must re-check containment on the
// EvalSymlinks result.
func TestSecurity_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows CI")
	}

	base := t.TempDir()
	modelsRoot := filepath.Join(base, "models")
	require.NoError(t, os.MkdirAll(modelsRoot, 0o755))

	// Put the symlink target OUTSIDE modelsRoot.
	secretDir := filepath.Join(base, "secrets")
	require.NoError(t, os.MkdirAll(secretDir, 0o755))
	secretFile := filepath.Join(secretDir, "passwd")
	require.NoError(t, os.WriteFile(secretFile, []byte("root:x:0:0:loot"), 0o600))

	// Create a symlink inside modelsRoot pointing to the secret outside.
	linkPath := filepath.Join(modelsRoot, "passwd-link")
	require.NoError(t, os.Symlink(secretFile, linkPath))

	s := NewSecurityFromRoot(modelsRoot)

	_, err := s.ValidateModelPath("passwd-link")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink target escapes models directory")
}

// TestIsWithin covers the filepath.Rel-based containment helper used
// by both Security and Client.
func TestIsWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.MkdirAll(root, 0o755))

	cases := []struct {
		name   string
		target string
		want   bool
	}{
		{"same path", root, true},
		{"direct child", filepath.Join(root, "a"), true},
		{"deep descendant", filepath.Join(root, "a", "b", "c"), true},
		{"sibling with prefix", root + "-evil", false},
		{"parent", filepath.Dir(root), false},
		{"unrelated", filepath.Join(t.TempDir(), "other"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isWithin(root, tc.target)
			assert.Equal(t, tc.want, got)
		})
	}
}
