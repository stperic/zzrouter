//go:build windows

package config

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Well-known SIDs that indicate broad access (the Windows equivalent of
// POSIX "world-readable"). If any of these SIDs have explicit READ_DATA
// access in the file's DACL, the file is treated as world-readable.
var broadAccessSIDs = [...]*windows.SID{
	trySID(windows.WinWorldSid),             // Everyone (S-1-1-0)
	trySID(windows.WinBuiltinUsersSid),      // BUILTIN\Users (S-1-5-32-545)
	trySID(windows.WinAuthenticatedUserSid), // NT AUTHORITY\Authenticated Users
}

func trySID(sidType windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	sid, err := windows.CreateWellKnownSid(sidType)
	if err != nil {
		return nil
	}
	return sid
}

// isWorldReadableImpl always returns false on Windows because os.FileInfo
// does not carry the file path needed for NTFS ACL inspection. Callers
// with a path should use isWorldReadablePath / isWorldReadablePathImpl.
func isWorldReadableImpl(_ os.FileInfo) bool {
	return false
}

func isWorldReadablePathImpl(path string) bool {
	return isWorldReadableByDACL(path)
}

// isWorldReadableByDACL checks whether path's NTFS DACL grants read access
// to a broad-access group. This is the Windows-native equivalent of the
// POSIX mode & 0004 check.
func isWorldReadableByDACL(path string) bool {
	// GetNamedSecurityInfo returns a Go-heap copy (LocalFree is handled
	// internally by x/sys/windows), so no explicit free is needed.
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false
	}

	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}

	for _, broadSID := range broadAccessSIDs {
		if broadSID == nil {
			continue
		}
		if daclGrantsRead(dacl, broadSID) {
			return true
		}
	}
	return false
}

// daclGrantsRead walks the DACL ACE list checking for ACCESS_ALLOWED_ACE
// entries that grant FILE_READ_DATA to the given SID.
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
