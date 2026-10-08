package config

import "os"

// isWorldReadable reports whether a file should be treated as world-readable
// for the purpose of "may contain secrets" warnings/rejections.
//
// On POSIX (Linux/macOS), we honor the standard mode bit check (mode & 0004).
// On Windows, Go's os.Stat synthesizes mode bits from the read-only attribute
// and file extension — it does not reflect the actual NTFS ACL. The FileInfo
// variant returns false on Windows; use isWorldReadablePath for real ACL checks.
func isWorldReadable(info os.FileInfo) bool {
	return isWorldReadableImpl(info)
}

// isWorldReadablePath checks whether the file at path is world-readable.
// On POSIX: stat + mode bit check. On Windows: queries the NTFS DACL for
// broad-access SIDs (Everyone, BUILTIN\Users, Authenticated Users).
func isWorldReadablePath(path string) bool {
	return isWorldReadablePathImpl(path)
}
