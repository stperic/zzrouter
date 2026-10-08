//go:build windows

package preflight

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// DiskSpace verifies the target directory has enough free space.
// If the directory doesn't exist yet, it walks up to the nearest existing ancestor.
func DiskSpace(dir string, requiredBytes uint64) error {
	check := dir
	for {
		if _, err := os.Stat(check); err == nil {
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")

	var freeBytesAvailable uint64
	dirPtr, _ := syscall.UTF16PtrFromString(check)

	ret, _, err := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if ret == 0 {
		return fmt.Errorf("failed to check disk space at %s: %w", dir, err)
	}

	if freeBytesAvailable < requiredBytes {
		return fmt.Errorf("insufficient disk space: %d MB available, %d MB required",
			freeBytesAvailable/(1024*1024), requiredBytes/(1024*1024))
	}
	return nil
}

// WritePermission verifies the process can write to the target directory.
// If the directory doesn't exist yet, it checks the nearest existing ancestor.
func WritePermission(dir string) error {
	// Walk up to find an existing directory
	check := dir
	for {
		info, err := os.Stat(check)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("path %s exists but is not a directory", check)
			}
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}

	// Try to create a temp file to verify write access
	probe := filepath.Join(check, ".zzrouter-write-probe")
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("%w on %s: %w", ErrNoWritePermission, check, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// MSVCRedistMinimum is the minimum MSVCP140.dll version required to run
// llama.cpp's pre-built Windows binaries (built with VS2022 / Clang 19+).
// Bare-metal Windows images (Vultr, Server 2022/2025) ship with MSVCP140
// 14.22 from the 2019 redist — too old. Process exits with NTSTATUS
// 0xC0000005 ACCESS_VIOLATION inside MSVCP140.dll at startup.
//
// This is the floor we check against. Bumping requires re-validating
// against the binaries we ship — keep aligned with the llama.cpp build
// toolchain version embedded in `llama-server --version` output.
//
//	major=14 minor=30 (Visual C++ 2022 redistributable v14.30)
const (
	msvcMajor = 14
	msvcMinor = 30
)

// systemDLL resolves a path inside the Windows system directory,
// honoring %SystemRoot% (defaults to C:\Windows when unset). Avoids
// hard-coding C:\Windows\System32 on customized installs.
func systemDLL(name string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", name)
}

// requiredVCDLLs lists the VS2022 redistributable DLLs that pre-built
// binaries (llama.cpp CUDA build is the motivating case) link against
// dynamically. msvcp140 holds the C++ standard library, vcruntime140
// the C runtime, and vcruntime140_1 the noexcept-throwing helpers added
// in the 2019 redist. Missing or too-old vcruntime140_1 produces the
// same NTSTATUS 0xC0000005 ACCESS_VIOLATION at process start as a stale
// MSVCP140 — both must be probed.
var requiredVCDLLs = []string{
	"msvcp140.dll",
	"vcruntime140.dll",
	"vcruntime140_1.dll",
}

// MSVCRedist verifies that every Visual C++ 2022 redistributable DLL
// that pre-built Windows binaries depend on is present and recent
// enough. Used by Windows installers for providers that ship VS2022-
// built binaries (notably llama.cpp's CUDA build).
//
// On non-Windows the call site is a no-op via build tags. The check
// fails preflight with an actionable hint pointing at the official
// Microsoft download URL when any DLL is missing or too old, rather
// than letting the install download 250 MB and crash at the verify
// step.
func MSVCRedist() Result {
	for _, name := range requiredVCDLLs {
		path := systemDLL(name)
		ver, err := readFileVersionMajorMinor(path)
		if err != nil {
			// Missing entirely is the older Server-Core / no-redist case
			// or a partial 2015 redist (which lacked vcruntime140_1).
			// Same hint applies — install vc_redist.x64.
			return Result{
				Check:   "msvc-redist",
				Passed:  false,
				Message: fmt.Sprintf("%s not readable at %s: %v", name, path, err),
				Hint:    "Install Visual C++ Redistributable 2015-2022 x64: https://aka.ms/vs/17/release/vc_redist.x64.exe (silent: vc_redist.x64.exe /install /quiet /norestart)",
			}
		}
		if ver.major < msvcMajor || (ver.major == msvcMajor && ver.minor < msvcMinor) {
			return Result{
				Check:   "msvc-redist",
				Passed:  false,
				Message: fmt.Sprintf("%s is %d.%d (need >= %d.%d): pre-built binaries will crash with NTSTATUS 0xC0000005 at startup", name, ver.major, ver.minor, msvcMajor, msvcMinor),
				Hint:    "Install Visual C++ Redistributable 2015-2022 x64: https://aka.ms/vs/17/release/vc_redist.x64.exe (silent: vc_redist.x64.exe /install /quiet /norestart)",
			}
		}
	}
	return Result{
		Check:   "msvc-redist",
		Passed:  true,
		Message: fmt.Sprintf("VC++ runtime DLLs present and >= %d.%d", msvcMajor, msvcMinor),
	}
}

type fileVersion struct{ major, minor int }

// readFileVersionMajorMinor reads a Windows PE file's FileVersion
// resource and returns its major.minor. Uses the version.dll API
// (GetFileVersionInfoSize → GetFileVersionInfo → VerQueryValue) — same
// path explorer.exe and Get-Item .VersionInfo.FileVersion go through.
//
// Memory-safety notes:
//   - `buf` is the heap-allocated VS_VERSIONINFO block. VerQueryValueW
//     returns a pointer INTO `buf`. Go's escape analysis only sees
//     `&buf[0]` cast to uintptr at call sites, which is not enough to
//     keep `buf` alive across the unsafe read. The read happens before
//     the explicit `runtime.KeepAlive(buf)`.
//   - The 4-byte FileVersionMS field is copied into a local before
//     return, so no pointer survives back to the caller.
func readFileVersionMajorMinor(path string) (fileVersion, error) {
	verDLL := syscall.NewLazyDLL("version.dll")
	if err := verDLL.Load(); err != nil {
		return fileVersion{}, fmt.Errorf("version.dll unavailable: %w", err)
	}
	getSize := verDLL.NewProc("GetFileVersionInfoSizeW")
	getInfo := verDLL.NewProc("GetFileVersionInfoW")
	queryValue := verDLL.NewProc("VerQueryValueW")

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fileVersion{}, err
	}

	size, _, callErr := getSize.Call(uintptr(unsafe.Pointer(pathPtr)), 0)
	if size == 0 {
		return fileVersion{}, fmt.Errorf("GetFileVersionInfoSize=0: %w", callErr)
	}

	buf := make([]byte, size)
	ret, _, callErr := getInfo.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		0,
		size,
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if ret == 0 {
		return fileVersion{}, fmt.Errorf("GetFileVersionInfo failed: %w", callErr)
	}

	// Pull the root VS_FIXEDFILEINFO. Sub-block "\\" returns it.
	subBlock, _ := syscall.UTF16PtrFromString(`\`)
	var fixedInfoPtr unsafe.Pointer
	var fixedInfoLen uint32
	ret, _, callErr = queryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(subBlock)),
		uintptr(unsafe.Pointer(&fixedInfoPtr)),
		uintptr(unsafe.Pointer(&fixedInfoLen)),
	)
	if ret == 0 || fixedInfoLen == 0 {
		return fileVersion{}, fmt.Errorf("VerQueryValue root failed: %w", callErr)
	}

	// VS_FIXEDFILEINFO layout: Signature, StrucVersion, FileVersionMS,
	// FileVersionLS, ProductVersionMS, ... — first 8 bytes are signature
	// + struct version, then file-version high/low DWORDs.
	if fixedInfoLen < 16 {
		return fileVersion{}, fmt.Errorf("VS_FIXEDFILEINFO too small: %d bytes", fixedInfoLen)
	}
	infoBytes := unsafe.Slice((*byte)(fixedInfoPtr), fixedInfoLen)
	// Copy out the field BEFORE KeepAlive — fixedInfoPtr lifetime ends
	// when buf is collected. binary.LittleEndian is clearer than
	// hand-shifting and matches the DWORD-low-order-first layout.
	hi := binary.LittleEndian.Uint32(infoBytes[8:12])
	runtime.KeepAlive(buf)
	runtime.KeepAlive(subBlock)
	runtime.KeepAlive(pathPtr)

	return fileVersion{major: int(hi >> 16), minor: int(hi & 0xFFFF)}, nil
}
