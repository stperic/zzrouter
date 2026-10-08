//go:build !windows

package service

// restrictEnvFilePerms is a no-op on Unix — the 0600 mode passed to
// os.WriteFile already restricts access to the file owner.
func restrictEnvFilePerms(_ string) error { return nil }
