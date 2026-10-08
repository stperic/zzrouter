//go:build windows

package service

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictEnvFilePerms tightens the DACL on the env file so that only the
// current user, SYSTEM, and the Administrators group can read it. Windows
// ignores the 0600 mode bits passed to os.WriteFile, so without this the
// default inherited DACL typically grants read access to broad groups
// (BUILTIN\Users or Authenticated Users).
//
// We replace the DACL wholesale (PROTECTED_DACL_SECURITY_INFORMATION) so
// that no inherited ACEs can re-open the file to other principals.
func restrictEnvFilePerms(path string) error {
	token := windows.GetCurrentProcessToken()

	userInfo, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get token user: %w", err)
	}
	userSID := userInfo.User.Sid

	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("create SYSTEM SID: %w", err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("create Administrators SID: %w", err)
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitGrantFullControl(userSID, windows.TRUSTEE_IS_USER),
		explicitGrantFullControl(systemSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
		explicitGrantFullControl(adminsSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}

	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build DACL: %w", err)
	}

	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("apply DACL to %s: %w", path, err)
	}
	return nil
}

func explicitGrantFullControl(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
