//go:build darwin

package gpu

// platformHint on macOS is mostly negative space: NVIDIA stopped
// shipping macOS drivers years ago, AMD GPU compute is unsupported
// outside Apple's own Metal stack, and Apple Silicon doesn't have
// a "driver" the user can install. We still return a sentence
// explaining the situation so the preflight report has something
// concrete to show — empty hints in StateHardwareNoDriver are
// noisier than a one-liner that tells the operator nothing they
// can do will help.
func platformHint(vendor Vendor) string {
	switch vendor {
	case VendorNVIDIA:
		return "NVIDIA drivers are not available on macOS. GPU-accelerated " +
			"inference on this machine requires a non-NVIDIA backend."
	case VendorAMD:
		return "AMD GPU compute is not supported on macOS. Use Apple Silicon " +
			"via Metal/MLX instead."
	default:
		// Apple Silicon's driver ships with macOS — there is no
		// install hint to give for VendorApple.
		return ""
	}
}
