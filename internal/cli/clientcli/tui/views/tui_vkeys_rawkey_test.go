package views

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/ui"
)

const testRawKey = "zzr_TESTONLY01234567890123456789012345678901234"

func newVkeysRawOverlay(t *testing.T) *VkeysViewModel {
	t.Helper()
	v := NewVkeysViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	v.loading = false
	v.mode = vkeysViewShowRaw
	v.rawKeyValue = testRawKey
	v.termWidth, v.termHeight = 100, 30
	return v
}

// stubClipboard swaps the native clipboard writer for the duration of a
// test and reports what it was handed.
func stubClipboard(t *testing.T, err error) *string {
	t.Helper()
	var mu sync.Mutex
	var got string
	prev := clipboardWrite
	clipboardWrite = func(s string) error {
		mu.Lock()
		defer mu.Unlock()
		got = s
		return err
	}
	t.Cleanup(func() { clipboardWrite = prev })
	return &got
}

// The secret is unrecoverable once the overlay closes, so keystrokes
// other than copy/save/dismiss must leave it on screen.
func TestVkeysRawKey_StrayKeystrokeDoesNotDismiss(t *testing.T) {
	for _, k := range []string{"x", "n", "d"} {
		v := newVkeysRawOverlay(t)

		updated, cmd := v.Update(keyPress(k))
		v = updated.(*VkeysViewModel)

		assert.Equal(t, vkeysViewShowRaw, v.mode, "key %q closed the overlay", k)
		assert.Equal(t, testRawKey, v.rawKeyValue, "key %q cleared the secret", k)
		assert.Nil(t, cmd)
	}
}

// The native OS clipboard is tried first: terminals refuse OSC 52 writes
// by default, so the escape alone silently does nothing.
func TestVkeysRawKey_CopyWritesNativeClipboardFirst(t *testing.T) {
	for _, k := range []string{"c", "C", "y", "Y"} {
		v := newVkeysRawOverlay(t)
		got := stubClipboard(t, nil)

		_, cmd := v.Update(keyPress(k))
		require.NotNil(t, cmd, "key %q issued no clipboard command", k)

		msg := cmd()
		require.IsType(t, vkeysCopiedMsg{}, msg)
		assert.Equal(t, testRawKey, *got, "key %q sent the wrong value", k)

		updated, _ := v.Update(msg)
		v = updated.(*VkeysViewModel)
		assert.Equal(t, vkeysViewShowRaw, v.mode)
		assert.Equal(t, "Copied to clipboard.", v.rawKeyFlash)
		assert.Contains(t, v.viewContent(), "Copied to clipboard.")
	}
}

// No pbcopy/xclip (an SSH session into a headless box) falls through to
// the terminal escape rather than reporting success.
func TestVkeysRawKey_CopyFallsBackToTerminalEscape(t *testing.T) {
	v := newVkeysRawOverlay(t)
	stubClipboard(t, errors.New("exec: pbcopy: not found"))

	_, cmd := v.Update(keyPress("c"))
	require.NotNil(t, cmd)

	updated, fallback := v.Update(cmd())
	v = updated.(*VkeysViewModel)

	require.NotNil(t, fallback, "a failed native write must still try OSC 52")
	assert.Equal(t, vkeysViewShowRaw, v.mode, "the secret stays up while copy is unconfirmed")
	assert.Contains(t, v.rawKeyFlash, "OSC 52")
	assert.Contains(t, v.rawKeyFlash, "press S", "the fallback must name the next escape hatch")
}

func TestVkeysRawKey_SaveWritesSecretTo0600File(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("HOME", cacheDir)
	t.Setenv("LocalAppData", cacheDir)
	v := newVkeysRawOverlay(t)

	_, cmd := v.Update(keyPress("s"))
	require.NotNil(t, cmd)

	msg, ok := cmd().(vkeysSavedKeyMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)

	data, err := os.ReadFile(msg.path)
	require.NoError(t, err)
	assert.Equal(t, testRawKey, strings.TrimSpace(string(data)))

	info, err := os.Stat(msg.path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a secret must not be world-readable")
	} else {
		// Windows mode bits do not express owner-only access; this test does not inspect ACLs.
		assert.True(t, info.Mode().IsRegular())
	}

	updated, _ := v.Update(msg)
	v = updated.(*VkeysViewModel)
	assert.Contains(t, v.rawKeyFlash, msg.path, "the operator needs the path to go get it")
}

func TestVkeysRawKey_ExplicitDismissClearsSecret(t *testing.T) {
	tests := map[string]tea.KeyPressMsg{
		"enter": {Code: tea.KeyEnter},
		"esc":   {Code: tea.KeyEscape},
		"q":     keyPress("q"),
	}
	for name, msg := range tests {
		v := newVkeysRawOverlay(t)
		v.rawKeyFlash = "Copied to clipboard."

		updated, _ := v.Update(msg)
		v = updated.(*VkeysViewModel)

		assert.Equal(t, vkeysViewList, v.mode, "key %q did not dismiss", name)
		assert.Empty(t, v.rawKeyValue)
		assert.Empty(t, v.rawKeyFlash)
	}
}

// Cell-motion capture routes drags to the app instead of selecting text,
// which would remove the manual fallback the overlay points at.
func TestVkeysRawKey_ReleasesMouseWhileSecretIsShown(t *testing.T) {
	v := newVkeysRawOverlay(t)
	assert.Equal(t, tea.MouseModeNone, v.View().MouseMode)

	v.mode = vkeysViewList
	assert.Equal(t, tea.MouseModeCellMotion, v.View().MouseMode,
		"the list still needs click and wheel handling")
}

// One status line, never a stack: the standing warning gives way to the
// outcome rather than sitting under it.
func TestVkeysRawKey_StatusLineReplacesWarning(t *testing.T) {
	v := newVkeysRawOverlay(t)
	out := v.viewContent()
	assert.Contains(t, out, "cannot be shown again")

	v.rawKeyFlash = "Copied to clipboard."
	out = v.viewContent()
	assert.Contains(t, out, "Copied to clipboard.")
	assert.NotContains(t, out, "cannot be shown again")
}
