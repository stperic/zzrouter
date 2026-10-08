//go:build linux

package gpu

// platformHint is the Linux-side install guidance string set.
// Linked into the binary only on Linux builds so we don't ship
// distro-specific package names on macOS or Windows.
func platformHint(vendor Vendor) string {
	switch vendor {
	case VendorNVIDIA:
		return "Install the proprietary NVIDIA driver for your distribution " +
			"(Debian/Ubuntu: 'sudo apt install nvidia-driver-XYZ' or use your " +
			"distro's driver manager; RHEL/Fedora: enable RPM Fusion and install " +
			"'akmod-nvidia'). Full instructions: https://www.nvidia.com/Download/index.aspx"
	case VendorAMD:
		return "Install the AMD ROCm stack for your distribution. See " +
			"https://rocm.docs.amd.com/projects/install-on-linux/en/latest/ " +
			"for the current supported kernel + package list."
	default:
		return ""
	}
}
