//go:build windows

package preflight

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMSVCRedist_RealSystem exercises MSVCRedist on the test runner's
// own VC++ runtime DLLs. The runner's redist version is platform-
// dependent so we can't pin Pass/Fail, but we CAN pin invariants:
//   - Result.Check is the canonical sentinel ("msvc-redist")
//   - Either Passed=true with a positive message, or Passed=false WITH
//     a non-empty Hint pointing operators at the fix
//
// The Hint check matters: an unactionable failure was the bug class
// the original feature existed to prevent.
func TestMSVCRedist_RealSystem(t *testing.T) {
	r := MSVCRedist()
	assert.Equal(t, "msvc-redist", r.Check)
	assert.NotEmpty(t, r.Message)
	if !r.Passed {
		assert.NotEmpty(t, r.Hint, "failed preflight must carry an actionable Hint")
	}
}

// TestMSVCRedistMinimum_FloorMatchesMotivatingCase pins the floor at
// 14.30. The 2019 redist (14.22) — what triggered the original
// 0xC0000005 crash on the Vultr Windows worker — must NOT satisfy the
// floor. Bumping msvcMajor/msvcMinor here means we've validated the
// shipped binaries against a newer toolchain.
func TestMSVCRedistMinimum_FloorMatchesMotivatingCase(t *testing.T) {
	require.Equal(t, 14, msvcMajor)
	require.Equal(t, 30, msvcMinor)

	// 14.22 (Vultr bare image) — must be below floor.
	bad := fileVersion{major: 14, minor: 22}
	assert.True(t, bad.major < msvcMajor || (bad.major == msvcMajor && bad.minor < msvcMinor),
		"14.22 must register as below the floor — that's the bug we're guarding against")

	// 14.30 — exactly at floor, must satisfy.
	atFloor := fileVersion{major: 14, minor: 30}
	assert.True(t, atFloor.major > msvcMajor || (atFloor.major == msvcMajor && atFloor.minor >= msvcMinor))
}

// TestRequiredVCDLLs_CoversThreeBlast pins the DLL set we probe.
// vcruntime140_1 was added in the 2019 redist and is required by
// noexcept-throwing VS2022 builds — its absence produces the SAME
// 0xC0000005 crash as a stale msvcp140, so probing only msvcp140 is
// insufficient. If a future bump drops one of these, that's a
// deliberate decision worth a test failure to surface.
func TestRequiredVCDLLs_CoversThreeBlast(t *testing.T) {
	assert.Contains(t, requiredVCDLLs, "msvcp140.dll")
	assert.Contains(t, requiredVCDLLs, "vcruntime140.dll")
	assert.Contains(t, requiredVCDLLs, "vcruntime140_1.dll")
}

// TestReadFileVersion_MissingFileFailsCleanly guards the error path
// when the target DLL doesn't exist. A bad path should surface as a
// returned error, never panic from the unsafe pointer plumbing.
func TestReadFileVersion_MissingFileFailsCleanly(t *testing.T) {
	_, err := readFileVersionMajorMinor(`C:\Windows\System32\does-not-exist-12345.dll`)
	require.Error(t, err)
}
