//go:build windows

package gpu

// platformHint is the Windows-side install guidance string set.
// Linked into the binary only on Windows builds — the strings
// reference the vendor download portals that Windows users
// expect rather than the Linux package-manager flow.
func platformHint(vendor Vendor) string {
	switch vendor {
	case VendorNVIDIA:
		return "Download and install the latest NVIDIA driver from " +
			"https://www.nvidia.com/Download/index.aspx and reboot."
	case VendorAMD:
		return "Download and install the latest AMD Adrenalin driver from " +
			"https://www.amd.com/en/support and reboot."
	default:
		return ""
	}
}
