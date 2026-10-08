package fsroot

import "runtime"

// Platform represents an OS/architecture combination. It lives in fsroot
// because install variant selection, plan metadata, and filesystem layout
// all consume it; keeping it in a stdlib-only leaf avoids cycles when the
// variant subpackage (which imports it) is itself called from root.
type Platform struct {
	OS   string `json:"os"`   // "darwin", "linux", "windows"
	Arch string `json:"arch"` // "amd64", "arm64"
}

// CurrentPlatform returns the platform of the running system.
func CurrentPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// String returns "os/arch".
func (p Platform) String() string {
	return p.OS + "/" + p.Arch
}
