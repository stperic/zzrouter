//go:build release

package process

// Release build: a missing LauncherSHA256 is a hard failure.
const devLauncherBypass = false
