package fsroot

import (
	"fmt"
	"os"
)

// Restore restores a backup after a failed upgrade.
func Restore(providerName string) error {
	backupDir := ProviderBackupDir(providerName)
	if _, err := os.Stat(backupDir); os.IsNotExist(err) {
		return fmt.Errorf("no backup found for %s", providerName)
	}

	installDir := ProviderDir(providerName)

	// Remove the failed install
	_ = os.RemoveAll(installDir)

	if err := os.Rename(backupDir, installDir); err != nil {
		return fmt.Errorf("failed to restore backup: %w", err)
	}
	return nil
}

// CleanupBackup removes the backup after a successful upgrade.
func CleanupBackup(providerName string) error {
	return os.RemoveAll(ProviderBackupDir(providerName))
}
