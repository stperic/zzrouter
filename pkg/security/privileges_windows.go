//go:build windows

package security

// setUmask is a no-op on Windows (different permission model)
func setUmask(mask int) int {
	return 0
}
