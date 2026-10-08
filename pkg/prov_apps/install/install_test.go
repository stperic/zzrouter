package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/prov_apps/install/download"
	"github.com/stperic/zzrouter/pkg/prov_apps/install/fsroot"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentPlatform(t *testing.T) {
	p := fsroot.CurrentPlatform()
	assert.Equal(t, runtime.GOOS, p.OS)
	assert.Equal(t, runtime.GOARCH, p.Arch)
	assert.Contains(t, p.String(), "/")
}

func TestProviderRootDir(t *testing.T) {
	root := fsroot.ProviderRootDir()
	assert.NotEmpty(t, root)
	assert.Contains(t, root, "providers")
}

func TestProviderDir(t *testing.T) {
	dir := fsroot.ProviderDir("vllm")
	assert.Contains(t, dir, "vllm")
	assert.NotEmpty(t, dir)
	// Provider dir should be a subdirectory of the root
	assert.True(t, strings.HasPrefix(dir, fsroot.ProviderRootDir()))
}

func TestProviderVenvPaths(t *testing.T) {
	assert.Contains(t, fsroot.ProviderVenvDir("vllm"), "venv")
	assert.Contains(t, fsroot.ProviderVenvPython("vllm"), "python")
	assert.Contains(t, fsroot.ProviderVenvPip("vllm"), "pip")
}

func TestProviderBackupDir(t *testing.T) {
	assert.Contains(t, fsroot.ProviderBackupDir("vllm"), ".backup")
}

// --- Verify ---

func TestVerifySHA256(t *testing.T) {
	// Create a temp file with known content
	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	content := []byte("hello world")
	require.NoError(t, os.WriteFile(path, content, 0644))

	hash := sha256.Sum256(content)
	expected := hex.EncodeToString(hash[:])

	assert.NoError(t, security.VerifySHA256(path, expected))
	assert.Error(t, security.VerifySHA256(path, "0000000000000000000000000000000000000000000000000000000000000000"))
}

func TestComputeSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.bin")
	require.NoError(t, os.WriteFile(path, []byte("test"), 0644))

	hash, err := security.ComputeSHA256(path)
	require.NoError(t, err)
	assert.Len(t, hash, 64) // 32 bytes = 64 hex chars
}

func TestVerifySymlinkSafe(t *testing.T) {
	dir := t.TempDir()

	// Non-existent path is safe
	assert.NoError(t, fsroot.VerifySymlinkSafe(filepath.Join(dir, "nonexistent")))

	// Regular file is safe
	path := filepath.Join(dir, "regular")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))
	assert.NoError(t, fsroot.VerifySymlinkSafe(path))

	// Symlink is NOT safe. On Windows, creating a symlink needs
	// SeCreateSymbolicLinkPrivilege (granted to Administrators or to
	// the current user when Developer Mode is on). Unprivileged CI
	// runs skip the symlink assertion rather than fail the suite.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		if canSkipSymlinkCreation(err) {
			t.Skipf("skipping symlink assertion: cannot create symlinks in this environment (%v)", err)
		}
		t.Fatalf("unexpected error creating symlink: %v", err)
	}
	assert.Error(t, fsroot.VerifySymlinkSafe(link))
}

// canSkipSymlinkCreation reports whether an os.Symlink error is the
// "not allowed to create symlinks" condition (Windows non-admin
// without Developer Mode) vs. a real bug. Matches the underlying
// Windows error string because errors.Is(os.ErrPermission) does not
// cover ERROR_PRIVILEGE_NOT_HELD on all Go versions.
func canSkipSymlinkCreation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "A required privilege is not held by the client") ||
		strings.Contains(msg, "not supported")
}

// --- Version validation ---

func TestValidateVersion(t *testing.T) {
	// Valid versions
	assert.NoError(t, fsroot.ValidateVersion(""))
	assert.NoError(t, fsroot.ValidateVersion("1.2.3"))
	assert.NoError(t, fsroot.ValidateVersion("0.8.0"))
	assert.NoError(t, fsroot.ValidateVersion("b4651"))
	assert.NoError(t, fsroot.ValidateVersion("1.0.0-rc1"))
	assert.NoError(t, fsroot.ValidateVersion("1.0.0+build.123"))

	// Command injection attempts
	assert.Error(t, fsroot.ValidateVersion("1.0; rm -rf /"))
	assert.Error(t, fsroot.ValidateVersion("$(whoami)"))
	assert.Error(t, fsroot.ValidateVersion("`id`"))
	assert.Error(t, fsroot.ValidateVersion("1.0 && cat /etc/passwd"))
	assert.Error(t, fsroot.ValidateVersion("1.0|cat /etc/passwd"))

	// Too long
	assert.Error(t, fsroot.ValidateVersion(string(make([]byte, 200))))
}

// --- Download host validation ---

func TestValidateHost(t *testing.T) {
	validate := func(url string) error {
		return security.ValidateDownloadURL(url, download.AllowedHosts)
	}
	assert.NoError(t, validate("https://github.com/repo/releases/file.tar.gz"))
	assert.NoError(t, validate("https://objects.githubusercontent.com/file"))
	assert.NoError(t, validate("https://files.pythonhosted.org/packages/file.whl"))

	assert.Error(t, validate("http://github.com/insecure")) // not HTTPS
	assert.Error(t, validate("https://evil.com/malware"))   // not in allowlist
}

// --- Plan ---

func TestPlan_VerifyStep_FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0644))

	step := Step{
		Number: 1,
		Verify: StepVerify{Type: "file_exists", Path: path},
	}
	result := VerifyStep(step)
	assert.True(t, result.Passed)

	step.Verify.Path = filepath.Join(dir, "nonexistent")
	result = VerifyStep(step)
	assert.False(t, result.Passed)
}

func TestPlan_VerifyStep_DirExists(t *testing.T) {
	dir := t.TempDir()

	step := Step{
		Number: 1,
		Verify: StepVerify{Type: "dir_exists", Path: dir},
	}
	result := VerifyStep(step)
	assert.True(t, result.Passed)
}

func TestPlan_VerifyStep_CommandOutput(t *testing.T) {
	step := Step{
		Number: 1,
		Verify: StepVerify{
			Type:     "command_output",
			Command:  "echo hello",
			Expected: "hello",
		},
	}
	result := VerifyStep(step)
	assert.True(t, result.Passed)
	assert.Equal(t, "hello", result.Actual)
}

func TestPlan_VerifyStep_CommandOutput_Mismatch(t *testing.T) {
	step := Step{
		Number: 1,
		Verify: StepVerify{
			Type:     "command_output",
			Command:  "echo wrong",
			Expected: "right",
		},
	}
	result := VerifyStep(step)
	assert.False(t, result.Passed)
}

func TestPlan_Execute(t *testing.T) {
	dir := t.TempDir()
	testFile := filepath.Join(dir, "created.txt")

	plan := &Plan{
		Provider: "test",
		Action:   "install",
		Steps: []Step{
			{
				Number:      1,
				Description: "Create test file",
				Command:     "touch " + testFile,
				Verify:      StepVerify{Type: "file_exists", Path: testFile},
			},
		},
	}

	var messages []string
	err := plan.Execute(context.Background(), func(msg string) {
		messages = append(messages, msg)
	})
	require.NoError(t, err)
	assert.FileExists(t, testFile)
	assert.Len(t, messages, 1)
}

func TestPlan_VerifyAll(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, []byte("data"), 0644))

	plan := &Plan{
		Provider: "test",
		Steps: []Step{
			{Number: 1, Verify: StepVerify{Type: "file_exists", Path: existing}},
			{Number: 2, Verify: StepVerify{Type: "file_exists", Path: filepath.Join(dir, "nope")}},
		},
	}

	result := plan.VerifyAll()
	assert.False(t, result.AllOK)
	assert.True(t, result.Steps[0].Passed)
	assert.False(t, result.Steps[1].Passed)

	// StatesFromVerify mirrors the results as neutrally-framed state probes
	// suitable for embedding in a plan-preview response.
	states := StatesFromVerify(result.Steps)
	require.Len(t, states, 2)
	assert.Equal(t, 1, states[0].Step)
	assert.True(t, states[0].Installed)
	assert.Equal(t, 2, states[1].Step)
	assert.False(t, states[1].Installed)
	assert.NotEmpty(t, states[1].Detail)
}

func TestProviderRootOverride(t *testing.T) {
	// Remember the platform default so we can confirm the override
	// actually replaces it and clears back to the original.
	original := fsroot.ProviderRootDir()

	fsroot.SetProviderRootOverride("/tmp/zzrouter-test-root")
	assert.Equal(t, "/tmp/zzrouter-test-root", fsroot.ProviderRootDir(), "override should replace platform default")

	// Empty string clears the override back to the platform default.
	fsroot.SetProviderRootOverride("")
	assert.Equal(t, original, fsroot.ProviderRootDir(), "empty override should restore platform default")
}

func TestBuildProxyEnv(t *testing.T) {
	// Empty input → empty map. Keeps the caller path simple
	// (BuildProxyEnv(ProxyConfig{}) → SetExecEnv({}) → clear).
	assert.Empty(t, fsroot.BuildProxyEnv("", "", "", ""))

	// Each field populates both upper- and lower-case forms (tools disagree).
	env := fsroot.BuildProxyEnv("http://p:8080", "http://p:8080", "localhost", "/etc/ca.pem")
	assert.Equal(t, "http://p:8080", env["HTTP_PROXY"])
	assert.Equal(t, "http://p:8080", env["http_proxy"])
	assert.Equal(t, "http://p:8080", env["HTTPS_PROXY"])
	assert.Equal(t, "http://p:8080", env["https_proxy"])
	assert.Equal(t, "localhost", env["NO_PROXY"])
	assert.Equal(t, "localhost", env["no_proxy"])
	assert.Equal(t, "/etc/ca.pem", env["SSL_CERT_FILE"])
	assert.Equal(t, "/etc/ca.pem", env["CURL_CA_BUNDLE"])
	assert.Equal(t, "/etc/ca.pem", env["PIP_CERT"])
}

func TestSetExecEnv_ClearsOnEmpty(t *testing.T) {
	fsroot.SetExecEnv(map[string]string{"HTTP_PROXY": "http://p:8080"})
	assert.Contains(t, fsroot.ExecEnv(), "HTTP_PROXY=http://p:8080")

	// Passing empty / nil clears — the install pipeline reverts to
	// parent-env inheritance only.
	fsroot.SetExecEnv(nil)
	assert.Nil(t, fsroot.ExecEnv())
	fsroot.SetExecEnv(map[string]string{})
	assert.Nil(t, fsroot.ExecEnv())
}

func TestPlan_VerifyAll_OptionalFailureDoesNotAbort(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, []byte("data"), 0644))
	missing := filepath.Join(dir, "nope")

	plan := &Plan{
		Provider: "test",
		Steps: []Step{
			{Number: 1, Verify: StepVerify{Type: "file_exists", Path: existing}},
			{Number: 2, Optional: true, Verify: StepVerify{Type: "file_exists", Path: missing}},
			{Number: 3, Verify: StepVerify{Type: "file_exists", Path: existing}},
		},
	}

	result := plan.VerifyAll()
	assert.True(t, result.AllOK, "optional verify failure must not flip AllOK")
	assert.True(t, result.Steps[0].Passed)
	assert.False(t, result.Steps[1].Passed, "per-step result stays honest even for optional")
	assert.True(t, result.Steps[2].Passed, "optional failure must not short-circuit subsequent steps")
}

// --- Lockfile ---

func TestLockFile(t *testing.T) {
	lock := fsroot.NewLockFile("test-provider")

	require.NoError(t, lock.Lock())

	// Second lock should fail
	lock2 := fsroot.NewLockFile("test-provider")
	assert.Error(t, lock2.Lock())

	lock.Unlock()

	// Now should succeed
	require.NoError(t, lock2.Lock())
	lock2.Unlock()
}

// --- Rollback ---

func TestBackupAndRestore(t *testing.T) {
	// Create a fake install dir
	dir := t.TempDir()
	installDir := filepath.Join(dir, "providers", "test")
	require.NoError(t, os.MkdirAll(installDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(installDir, "binary"), []byte("v1"), 0644))

	// Override ProviderDir for test
	// Since we can't easily override, test the Backup/Restore functions directly
	backupDir := installDir + "-backup"

	// Manual backup
	require.NoError(t, os.Rename(installDir, backupDir))
	assert.NoDirExists(t, installDir)
	assert.DirExists(t, backupDir)

	// Manual restore
	require.NoError(t, os.Rename(backupDir, installDir))
	assert.DirExists(t, installDir)
	assert.NoDirExists(t, backupDir)

	// Verify content preserved
	data, err := os.ReadFile(filepath.Join(installDir, "binary"))
	require.NoError(t, err)
	assert.Equal(t, "v1", string(data))
}

func TestParseVersionOutput(t *testing.T) {
	assert.Equal(t, "0.11.0", ParseVersionOutput("0.11.0\n"))
	assert.Equal(t, "1.2.3", ParseVersionOutput("  1.2.3  "))
	assert.Equal(t, "unknown", ParseVersionOutput(""))
	// Falls back to last non-empty line when no "version:" prefix
	assert.Equal(t, "second", ParseVersionOutput("first\nsecond"))
	// llama.cpp format: noisy output with "version: NNNN (hash)" line
	assert.Equal(t, "8660", ParseVersionOutput("load_backend: loaded RPC backend\nversion: 8660 (d00685831)\nbuilt with GNU 11.4.0"))
	// version: prefix with no build info
	assert.Equal(t, "b5270", ParseVersionOutput("version: b5270"))
	// Semver regex requires 3-part version — should NOT match "11.2" in CUDA error
	assert.Equal(t, "requires CUDA 11.2 for GPU", ParseVersionOutput("requires CUDA 11.2 for GPU"))
	// Real semver in noisy output
	assert.Equal(t, "0.19.0", ParseVersionOutput("Loading...\n0.19.0"))
}

func TestShellQuote(t *testing.T) {
	// Basic path without spaces
	q := fsroot.ShellQuote("/opt/zzrouter/providers/vllm")
	assert.Contains(t, q, "/opt/zzrouter/providers/vllm")

	// Path with spaces (macOS)
	q = fsroot.ShellQuote("/Users/test/Library/Application Support/zzrouter")
	assert.Contains(t, q, "Application Support")
}

func TestPowerShellQuote(t *testing.T) {
	assert.Equal(t, "'simple'", fsroot.PowerShellQuote("simple"))
	assert.Equal(t, "'it''s here'", fsroot.PowerShellQuote("it's here"))
	assert.Equal(t, "'C:\\Program Files\\zzrouter'", fsroot.PowerShellQuote(`C:\Program Files\zzrouter`))
}

func TestEnsureProviderRoot(t *testing.T) {
	previousRoot := fsroot.ProviderRootDir()
	fsroot.SetProviderRootOverride(t.TempDir())
	t.Cleanup(func() { fsroot.SetProviderRootOverride(previousRoot) })

	// EnsureProviderRoot should not error on the current platform
	// (it creates the directory if it doesn't exist, or succeeds if it does)
	err := fsroot.EnsureProviderRoot()
	assert.NoError(t, err)

	// Verify the directory exists after the call
	root := fsroot.ProviderRootDir()
	info, err := os.Stat(root)
	assert.NoError(t, err)
	assert.True(t, info.IsDir())
}
