//go:build windows

package gpu

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// probePlatform reads the Windows PnP registry directly. We
// deliberately avoid spawning powershell / Get-PnpDevice here:
// every powershell spawn loads .NET, and .NET's UCRT dependency
// has been known to surface a "Install Visual C++ Redistributable"
// popup on machines whose runtime chain is incomplete. A plain
// registry read is free of that failure mode — it runs in-process
// via advapi32 and has no .NET or subprocess involvement.
//
// HKLM\SYSTEM\CurrentControlSet\Enum\PCI is world-readable, so
// elevation is never required for any of these lookups.
func probePlatform(ctx context.Context, vendor Vendor) (Detection, error) {
	switch vendor {
	case VendorNVIDIA, VendorAMD:
		return probePCIVendorWindows(vendor), nil
	case VendorApple:
		// Apple Silicon never appears on Windows.
		return Detection{Vendor: vendor, State: StateAbsent}, nil
	default:
		return Detection{Vendor: vendor, State: StateUnknown},
			fmt.Errorf("gpu: unsupported vendor %q", vendor)
	}
}

const (
	// pnpDisplayClassGUID is Windows' display-adapter class. Used
	// to filter out non-display PCI functions exposed by the same
	// vendor (audio controllers on NVIDIA HDMI, USB controllers on
	// AMD cards, etc.).
	pnpDisplayClassGUID = "{4d36e968-e325-11ce-bfc1-08002be10318}"

	// configFlagBroken combines the Windows CONFIGFLAG bits that
	// indicate a device is not functional: disabled by the user,
	// needs reinstall, or the driver install failed outright.
	// See DEVPKEY_Device_ConfigFlags in setupapi.
	configFlagDisabled      uint64 = 0x00000001 // CONFIGFLAG_DISABLED
	configFlagReinstall     uint64 = 0x00000020 // CONFIGFLAG_REINSTALL
	configFlagFailedInstall uint64 = 0x00000040 // CONFIGFLAG_FAILEDINSTALL
	configFlagBroken               = configFlagDisabled | configFlagReinstall | configFlagFailedInstall
)

// probePCIVendorWindows walks every PCI device under the requested
// vendor prefix, filters to display-class functions, and returns a
// Detection reflecting the best instance's driver state.
//
// Tri-state decision:
//
//   - No matching hardware ID in the registry → StateAbsent.
//   - Hardware ID present but no vendor driver bound (the device
//     is running on BasicDisplay / vgasave, or Service is empty) →
//     StateHardwareNoDriver with an install hint.
//   - Hardware ID present, vendor driver bound, ConfigFlags clean
//     → StateDriverOK with Name + DriverVersion from the Class
//     subkey.
func probePCIVendorWindows(vendor Vendor) Detection {
	d := Detection{Vendor: vendor, State: StateAbsent}

	var pciVendor string
	switch vendor {
	case VendorNVIDIA:
		pciVendor = PCIVendorNVIDIA
	case VendorAMD:
		pciVendor = PCIVendorAMD
	default:
		return d
	}

	instances, err := enumPCIDisplayInstances(pciVendor)
	if err != nil || len(instances) == 0 {
		return d
	}
	d.State = StateHardwareNoDriver
	d.Count = len(instances)

	// Scan for the first functioning instance. If none are
	// functioning we stay in StateHardwareNoDriver and populate the
	// install hint from the Detection's vendor.
	for _, inst := range instances {
		if !isVendorDriverService(inst.Service) {
			continue
		}
		if inst.ConfigFlags&configFlagBroken != 0 {
			continue
		}
		d.State = StateDriverOK
		if inst.DeviceDesc != "" {
			d.Name = inst.DeviceDesc
		}
		if inst.DriverVersion != "" {
			d.DriverVersion = inst.DriverVersion
		}
		return d
	}

	d.InstallHint = InstallHint(vendor)
	return d
}

// pciInstance is a single row read from HKLM\…\Enum\PCI\VEN_xxxx\<instance>.
type pciInstance struct {
	HardwareID    string // "VEN_10DE&DEV_1B06&SUBSYS_11B110DE&REV_A1"
	InstanceID    string // "4&3B6F4F5E&0&0008"
	ClassGUID     string
	Service       string
	ConfigFlags   uint64
	DeviceDesc    string
	DriverVersion string
}

const pciEnumRoot = `SYSTEM\CurrentControlSet\Enum\PCI`

// enumPCIDisplayInstances returns every display-class device whose
// hardware ID begins with VEN_{pciVendor}. The vendor prefix is
// matched case-insensitively because Windows stores keys upper-case
// but we want callers to pass lowercase IDs for Linux parity.
func enumPCIDisplayInstances(pciVendor string) ([]pciInstance, error) {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, pciEnumRoot, registry.READ)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	// ReadSubKeyNames(-1) returns every child in one call; PCI trees
	// on typical desktops top out at a few dozen entries.
	hwIDs, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}

	prefix := "VEN_" + strings.ToUpper(pciVendor)
	var results []pciInstance
	for _, hwID := range hwIDs {
		if !strings.HasPrefix(strings.ToUpper(hwID), prefix) {
			continue
		}
		results = append(results, readHardwareIDInstances(hwID)...)
	}
	return results, nil
}

// readHardwareIDInstances walks the device-instance subkeys under a
// single hardware ID (e.g. VEN_10DE&DEV_1B06&...) and returns one
// pciInstance per instance that belongs to the display class. Any
// registry error on an individual instance is swallowed — we want a
// partial answer over no answer.
func readHardwareIDInstances(hwID string) []pciInstance {
	hwKey, err := registry.OpenKey(registry.LOCAL_MACHINE, pciEnumRoot+`\`+hwID, registry.READ)
	if err != nil {
		return nil
	}
	instanceNames, err := hwKey.ReadSubKeyNames(-1)
	hwKey.Close()
	if err != nil {
		return nil
	}

	var out []pciInstance
	for _, instName := range instanceNames {
		inst, ok := readPCIInstance(hwID, instName)
		if !ok {
			continue
		}
		if !strings.EqualFold(inst.ClassGUID, pnpDisplayClassGUID) {
			// Skip audio, USB, SMBus, etc. that share the vendor ID.
			continue
		}
		out = append(out, inst)
	}
	return out
}

// readPCIInstance opens a single device instance key and extracts
// the properties we care about. Missing values are tolerated — the
// caller falls back gracefully on zero values.
func readPCIInstance(hwID, instName string) (pciInstance, bool) {
	inst := pciInstance{HardwareID: hwID, InstanceID: instName}

	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		pciEnumRoot+`\`+hwID+`\`+instName, registry.READ)
	if err != nil {
		return inst, false
	}
	defer key.Close()

	if v, _, err := key.GetStringValue("ClassGUID"); err == nil {
		inst.ClassGUID = v
	}
	if v, _, err := key.GetStringValue("Service"); err == nil {
		inst.Service = v
	}
	if v, _, err := key.GetStringValue("DeviceDesc"); err == nil {
		inst.DeviceDesc = stripRegIndirectPrefix(v)
	}
	if v, _, err := key.GetIntegerValue("ConfigFlags"); err == nil {
		inst.ConfigFlags = v
	}

	// The Driver value is a reference like "{class-guid}\0000" that
	// points into HKLM\SYSTEM\CurrentControlSet\Control\Class, where
	// the DriverVersion and other INF-derived properties live.
	if driverRef, _, err := key.GetStringValue("Driver"); err == nil && driverRef != "" {
		inst.DriverVersion = readClassDriverVersion(driverRef)
	}

	return inst, true
}

// readClassDriverVersion follows a \Driver reference into
// Control\Class\{class-guid}\NNNN and reads DriverVersion.
func readClassDriverVersion(driverRef string) string {
	classKey, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Class\`+driverRef, registry.READ)
	if err != nil {
		return ""
	}
	defer classKey.Close()
	if v, _, err := classKey.GetStringValue("DriverVersion"); err == nil {
		return v
	}
	return ""
}

// knownVendorDriverServices is the allow-list of Windows display
// driver service names that unlock the NVML / ADLX / HIP runtimes
// we care about. This is deliberately an allow-list rather than
// a deny-list of fallback drivers: an unknown OEM service name
// is more likely to be a misconfiguration we shouldn't silently
// accept than a real driver we forgot about. Add new entries
// here when a new vendor service ships, with a comment naming
// the chip family.
var knownVendorDriverServices = map[string]struct{}{
	"nvlddmkm": {}, // NVIDIA — every modern Windows driver
	"amdkmdag": {}, // AMD — current Adrenalin display driver
	"amdgpu":   {}, // AMD — newer driver branch (Linux name reused on Windows)
	"atikmdag": {}, // AMD — legacy "ATI" branding, still shipped on older cards
	"radeon":   {}, // AMD — pre-Adrenalin desktop driver
}

// isVendorDriverService reports whether the bound driver service
// is a real vendor display driver (nvlddmkm, amdkmdag, amdgpu,
// atikmdag, radeon) rather than one of Windows' fallback display
// drivers (BasicDisplay, vgasave, basicrender) or some unknown
// generic. The vendor driver is what unlocks the NVML / ADLX /
// HIP runtimes we probe for — fallback drivers expose a
// framebuffer only.
func isVendorDriverService(service string) bool {
	if service == "" {
		return false
	}
	_, ok := knownVendorDriverServices[strings.ToLower(service)]
	return ok
}

// stripRegIndirectPrefix strips the Windows "@inf-file,%key%;"
// indirect-string prefix that's often present on DeviceDesc. The
// display name is the literal text after the last semicolon.
func stripRegIndirectPrefix(s string) string {
	if idx := strings.LastIndex(s, ";"); idx >= 0 && idx+1 < len(s) {
		return s[idx+1:]
	}
	return s
}
