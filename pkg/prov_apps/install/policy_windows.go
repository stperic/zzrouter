package install

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_DELETE_CHILD is defined in WinNT.h but not exported by x/sys/windows.
// https://learn.microsoft.com/en-us/windows/win32/fileio/file-access-rights-constants
const fileDeleteChild windows.ACCESS_MASK = 0x40

func checkPolicyPath(path string) (os.FileInfo, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	if user.User.Sid.Equals(system) || token.IsElevated() {
		return nil, fmt.Errorf("LocalSystem or elevated services cannot establish an override boundary")
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	trusted := func(sid *windows.SID) bool { return sid.Equals(system) || sid.Equals(administrators) }
	var leaf os.FileInfo
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("policy path contains a reparse link")
		}
		attributes, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return nil, err
		}
		flags, err := windows.GetFileAttributes(attributes)
		if err != nil || flags&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, fmt.Errorf("policy path contains a reparse point")
		}
		descriptor, err := windows.GetNamedSecurityInfo(current, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return nil, err
		}
		owner, _, err := descriptor.Owner()
		if err != nil || owner == nil || !trusted(owner) {
			return nil, fmt.Errorf("policy path must be owned by SYSTEM or Administrators")
		}
		acl, _, err := descriptor.DACL()
		if err != nil || acl == nil {
			return nil, fmt.Errorf("protected policy DACL required")
		}
		for index := uint32(0); index < uint32(acl.AceCount); index++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, index, &ace); err != nil {
				return nil, err
			}
			if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				continue
			}
			if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
				continue
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
				return nil, fmt.Errorf("unrecognized policy ACE")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			// Creation of a new sibling cannot replace an existing protected ancestor.
			mask := windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE | fileDeleteChild
			if !info.IsDir() {
				mask |= windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA
			}
			if ace.Mask&mask != 0 && !trusted(sid) {
				return nil, fmt.Errorf("policy path grants service or untrusted writes")
			}
		}
		if current == path {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("regular policy file required")
			}
			leaf = info
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return leaf, nil
}
