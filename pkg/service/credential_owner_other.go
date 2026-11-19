//go:build !windows

package service

// CheckInstallationCredentialOwner has no SCM account to validate off Windows.
func CheckInstallationCredentialOwner(_ string) error { return nil }
