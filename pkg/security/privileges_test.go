package security

import (
	"os"
	"runtime"
	"testing"
)

func TestCheckNotRoot(t *testing.T) {
	// Skip on Windows - different privilege model
	if runtime.GOOS == "windows" {
		t.Skip("Skipping root check test on Windows")
	}

	err := CheckNotRoot()

	// If we're running as root (UID 0), we expect an error
	if os.Getuid() == 0 {
		if err == nil {
			t.Error("CheckNotRoot() should return error when running as root")
		}
	} else {
		// If we're not root, we expect no error
		if err != nil {
			t.Errorf("CheckNotRoot() returned unexpected error for non-root user: %v", err)
		}
	}
}

func TestCheckNotRootWithBypass(t *testing.T) {
	// Skip on Windows
	if runtime.GOOS == "windows" {
		t.Skip("Skipping root check test on Windows")
	}

	// Test with bypass enabled
	t.Setenv("ZZROUTER_ALLOW_ROOT", "1")

	err := CheckNotRootWithBypass()

	// With bypass enabled, should never error (even if root)
	if err != nil {
		t.Errorf("CheckNotRootWithBypass() should not error with ZZROUTER_ALLOW_ROOT=1: %v", err)
	}
}

func TestCheckNotRootWithBypassDisabled(t *testing.T) {
	// Skip on Windows
	if runtime.GOOS == "windows" {
		t.Skip("Skipping root check test on Windows")
	}

	// Ensure bypass is disabled
	t.Setenv("ZZROUTER_ALLOW_ROOT", "")

	err := CheckNotRootWithBypass()

	// Behavior should match CheckNotRoot when bypass is disabled
	if os.Getuid() == 0 {
		if err == nil {
			t.Error("CheckNotRootWithBypass() should return error when running as root without bypass")
		}
	} else {
		if err != nil {
			t.Errorf("CheckNotRootWithBypass() returned unexpected error for non-root user: %v", err)
		}
	}
}

func TestSetSecureUmask(t *testing.T) {
	// Skip on Windows
	if runtime.GOOS == "windows" {
		t.Skip("Skipping umask test on Windows")
	}

	// Get current umask, set secure umask, then restore
	oldUmask := SetSecureUmask()

	// Verify umask was set to 0027
	currentUmask := setUmask(oldUmask) // This also restores old umask

	if currentUmask != 0027 {
		t.Errorf("SetSecureUmask() set umask to %04o, expected 0027", currentUmask)
	}
}
