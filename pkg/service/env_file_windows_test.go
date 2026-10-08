//go:build windows

package service

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// assertEnvFilePermsRestricted verifies the env file's NTFS DACL does not
// grant READ_DATA to broadly-accessible SIDs (Everyone, BUILTIN\Users,
// Authenticated Users). Unix mode bits are not meaningful on Windows, so
// we inspect the DACL directly.
func assertEnvFilePermsRestricted(t *testing.T, path string) {
	t.Helper()

	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	require.NoError(t, err)

	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl, "file must have an explicit DACL after restrictEnvFilePerms")

	broadSIDs := map[string]windows.WELL_KNOWN_SID_TYPE{
		"Everyone":            windows.WinWorldSid,
		"BUILTIN\\Users":      windows.WinBuiltinUsersSid,
		"Authenticated Users": windows.WinAuthenticatedUserSid,
	}
	for name, sidType := range broadSIDs {
		sid, err := windows.CreateWellKnownSid(sidType)
		require.NoError(t, err, "create SID %s", name)
		assert.Falsef(t, daclGrantsRead(dacl, sid),
			"DACL must not grant read to %s", name)
	}
}

// daclGrantsRead mirrors the walk in pkg/config/permcheck_windows.go.
// It returns true if any ACCESS_ALLOWED_ACE in the DACL grants FILE_READ_DATA
// to the given SID.
func daclGrantsRead(dacl *windows.ACL, sid *windows.SID) bool {
	const fileReadData = 0x0001

	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID.Equals(sid) && (ace.Mask&fileReadData) != 0 {
			return true
		}
	}
	return false
}
