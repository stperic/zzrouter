//go:build windows

package security

import "os"

// GiveOwner changes nothing on Windows, where access follows the DACL that
// RestrictFilePermissions and RestrictDirectoryPermissions set; it reports a
// missing reference as other platforms do.
func GiveOwner(_, reference string) error {
	_, err := os.Stat(reference)
	return err
}
