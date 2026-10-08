package gpu

// InstallHint returns a short actionable sentence telling the user
// how to get a working driver stack for the given vendor on this
// platform. It is populated into Detection.InstallHint when the
// probe result is StateHardwareNoDriver.
//
// The strings are deliberately conservative: we link to the vendor's
// own installer page rather than prescribing a package command,
// because distro-specific instructions go stale quickly and a broken
// driver install can brick a machine.
//
// The actual hint text lives in the per-platform hint_*.go files
// next to this one — Linux gets the apt/dnf instructions, Windows
// gets the nvidia.com / amd.com download links, Darwin gets a
// "no driver" message because neither vendor ships macOS drivers
// anymore. Build tags select the right strings at compile time so
// the macOS binary doesn't carry the Linux instructions and vice
// versa.
func InstallHint(vendor Vendor) string {
	return platformHint(vendor)
}
