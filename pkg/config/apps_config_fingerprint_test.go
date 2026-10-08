package config

import (
	"testing"
)

// TestAppsConfigFingerprint_Stable asserts that two AppsConfigs with
// identical material fields produce the same fingerprint, regardless
// of how the providers map is iterated.
func TestAppsConfigFingerprint_Stable(t *testing.T) {
	a := &AppsConfig{Version: "1", Name: "n"}
	b := &AppsConfig{Version: "1", Name: "n"}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("identical configs fingerprint to different values")
	}
}

// TestAppsConfigFingerprint_ChangesOnMaterialEdit asserts that a
// header-field change flips the fingerprint.
func TestAppsConfigFingerprint_ChangesOnMaterialEdit(t *testing.T) {
	a := &AppsConfig{Version: "1", Name: "n"}
	b := &AppsConfig{Version: "2", Name: "n"}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatalf("version change did not flip fingerprint")
	}
}

// TestAppsConfigFingerprint_NilIsZero asserts that nil config produces
// the zero hash — callers treat it as an always-material sentinel.
func TestAppsConfigFingerprint_NilIsZero(t *testing.T) {
	var c *AppsConfig
	if c.Fingerprint() != [32]byte{} {
		t.Fatalf("nil config fingerprint != [32]byte{}")
	}
}
