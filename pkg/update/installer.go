package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// maxBinarySize is the maximum allowed size for extracted binaries (500MB)
const maxBinarySize = 500 * 1024 * 1024

// ErrNoBackupsAvailable is returned by Rollback when no prior install left a
// backup behind. Callers (HTTP layer) classify this as 404 — the resource the
// caller is asking to restore does not exist.
var ErrNoBackupsAvailable = errors.New("no backups available for rollback")

// errBinaryNotInArchive reports that a release archive carries no member
// for the requested binary. Fatal for the binary being updated; for the
// launcher it just means the release predates shipping one.
var errBinaryNotInArchive = errors.New("binary not found in archive")

// Binary names as they appear on disk, without the platform suffix a
// release archive adds to its members.
const (
	nodeBinaryName     = "zzrouter-node"
	launcherBinaryName = "zzrouter-launcher"
)

// releaseOSNames are the platform names a release archive can carry in a
// member name ("zzrouter-node-linux-amd64"). Matched against the name
// rather than the running platform: recognising the member is a parsing
// job, and the archive that reached us is this platform's already.
var releaseOSNames = []string{"linux", "darwin", "windows"}

// Installer handles installation of downloaded updates
type Installer struct {
	backupDir            string
	keepPreviousVersions int
	currentExePath       string // Override for testing (empty = auto-detect)
}

// NewInstaller creates a new installer
func NewInstaller(backupDir string, keepPreviousVersions int) *Installer {
	return &Installer{
		backupDir:            backupDir,
		keepPreviousVersions: keepPreviousVersions,
	}
}

// SetCurrentExecutable sets a custom current executable path (for testing)
func (i *Installer) SetCurrentExecutable(path string) {
	i.currentExePath = path
}

// GetCurrentExecutable returns the current executable path
func (i *Installer) getCurrentExecutable() (string, error) {
	if i.currentExePath != "" {
		return i.currentExePath, nil
	}

	currentExe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get current executable: %w", err)
	}

	// Resolve symlinks
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("failed to resolve symlinks: %w", err)
	}

	return currentExe, nil
}

// Install installs a new binary, creating a backup of the current one.
//
// expected is the version the release claims to carry; the extracted
// binary is run and has to agree before anything on disk is replaced.
// Pass nil only when there is nothing to compare against.
func (i *Installer) Install(ctx context.Context, archivePath string, expected *version.Version) (*InstallResult, error) {
	result := &InstallResult{
		Success: false,
	}

	// Get current executable path
	currentExe, err := i.getCurrentExecutable()
	if err != nil {
		return nil, err
	}

	// A managed install replaces a directory and a symlink rather than a
	// file, so it diverges here and shares only the extract-and-prove
	// half below.
	if l := detectLayout(currentExe); l != nil {
		return i.installManaged(ctx, l, archivePath, expected)
	}

	result.NewPath = currentExe

	if err := checkWritable(currentExe); err != nil {
		return nil, err
	}

	// Ensure backup directory exists
	if err := os.MkdirAll(i.backupDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Extract the new binary from archive
	extractedPath, err := i.extractBinary(archivePath, currentExe)
	if err != nil {
		return nil, fmt.Errorf("failed to extract binary: %w", err)
	}

	// Run it before committing to it. Everything below this line is a
	// change to what the node executes next.
	if err := smokeTest(ctx, extractedPath, expected); err != nil {
		return nil, err
	}

	launcherTarget := launcherSibling(currentExe)
	extractedLauncher, err := i.extractLauncher(archivePath, launcherTarget)
	if err != nil {
		return nil, err
	}

	// One stamp for both backups: it is the only thing that says the
	// launcher saved here belongs with the node saved here.
	stamp := utils.Now().Format(backupStampLayout)

	// Create backup of current binary
	backupPath, err := i.createBackupAt(currentExe, stamp)
	if err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}
	result.BackupPath = backupPath

	var launcherBackup string
	if extractedLauncher != "" {
		if _, statErr := os.Stat(launcherTarget); statErr == nil {
			if launcherBackup, err = i.createBackupAt(launcherTarget, stamp); err != nil {
				return nil, fmt.Errorf("failed to create launcher backup: %w", err)
			}
		}
		// Launcher first, node last. The node binary carries the hash
		// the launcher has to satisfy, so the pair is mismatched only
		// between these two lines — and replacing the node is what
		// triggers the restart that closes the window.
		if err := i.replaceBinary(launcherTarget, extractedLauncher); err != nil {
			return nil, fmt.Errorf("failed to replace launcher: %w", err)
		}
		result.LauncherPath = launcherTarget
		result.LauncherBackupPath = launcherBackup
	}

	// Replace the binary
	if err := i.replaceBinary(currentExe, extractedPath); err != nil {
		// Try to restore from backup
		if restoreErr := i.restoreBackup(backupPath, currentExe); restoreErr != nil {
			slog.Error("failed to restore backup after replace failure", "err", restoreErr)
		}
		if launcherBackup != "" {
			if restoreErr := i.restoreBackup(launcherBackup, launcherTarget); restoreErr != nil {
				slog.Error("failed to restore launcher backup after replace failure", "err", restoreErr)
			}
		}
		return nil, fmt.Errorf("failed to replace binary: %w", err)
	}

	// Clean up old backups
	if err := i.cleanupOldBackups(); err != nil {
		// Non-fatal error
		slog.Warn("failed to cleanup old backups", "err", err)
	}

	result.Success = true
	return result, nil
}

// extractLauncher pulls the launcher out of the archive when the binary
// being updated is the node. An archive without one is not an error: a
// release built before the launcher shipped also embeds no launcher
// hash, so the launcher already on disk stays acceptable to it.
func (i *Installer) extractLauncher(archivePath, launcherTarget string) (string, error) {
	if launcherTarget == "" {
		return "", nil
	}

	extracted, err := i.extractBinary(archivePath, launcherTarget)
	switch {
	case errors.Is(err, errBinaryNotInArchive):
		slog.Info("release ships no launcher; keeping the installed one", "launcher", launcherTarget)
		return "", nil
	case err != nil:
		return "", fmt.Errorf("failed to extract launcher: %w", err)
	}
	return extracted, nil
}

// extractBinary extracts the binary from an archive (tar.gz or zip)
func (i *Installer) extractBinary(archivePath, targetName string) (string, error) {
	// Determine archive type by extension
	lowerPath := strings.ToLower(archivePath)
	if strings.HasSuffix(lowerPath, ".zip") {
		return i.extractBinaryFromZip(archivePath, targetName)
	}
	return i.extractBinaryFromTarGz(archivePath, targetName)
}

// extractBinaryFromTarGz extracts the binary from a tar.gz archive
func (i *Installer) extractBinaryFromTarGz(archivePath, targetName string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("failed to open archive: %w", err)
	}
	defer func() { _ = file.Close() }()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() { _ = gzReader.Close() }()

	tarReader := tar.NewReader(gzReader)

	// Determine what binary name we're looking for
	binaryName := filepath.Base(targetName)
	if runtime.GOOS == "windows" && !strings.HasSuffix(binaryName, ".exe") {
		binaryName += ".exe"
	}

	tempDir := filepath.Dir(archivePath)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Only process regular files - skip directories, symlinks, hardlinks, etc.
		// This prevents symlink attacks where a malicious archive could contain
		// symlinks pointing to sensitive system files
		if header.Typeflag != tar.TypeReg {
			continue
		}

		// Validate file size to prevent decompression bombs
		if header.Size > maxBinarySize {
			return "", fmt.Errorf("file %s exceeds maximum allowed size (%d bytes)", header.Name, maxBinarySize)
		}

		// Use path.Base for tar entries (always uses forward slashes)
		// This prevents path traversal attacks like "../../etc/passwd"
		entryName := path.Base(header.Name)

		// Validate entry name has no path traversal
		if strings.Contains(entryName, "..") || strings.ContainsAny(entryName, `/\`) {
			continue
		}

		// Check if this is the binary we want
		if !i.isBinaryMatch(entryName, binaryName) {
			continue
		}

		// Extract the binary with size limit
		extractedPath := filepath.Join(tempDir, "new-"+binaryName)
		limitedReader := io.LimitReader(tarReader, maxBinarySize)
		if err := i.extractToFile(extractedPath, limitedReader, header.FileInfo().Mode()); err != nil {
			return "", err
		}

		return extractedPath, nil
	}

	return "", errBinaryNotInArchive
}

// extractBinaryFromZip extracts the binary from a zip archive (Windows)
func (i *Installer) extractBinaryFromZip(archivePath, targetName string) (string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("failed to open zip archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Determine what binary name we're looking for
	binaryName := filepath.Base(targetName)
	if runtime.GOOS == "windows" && !strings.HasSuffix(binaryName, ".exe") {
		binaryName += ".exe"
	}

	tempDir := filepath.Dir(archivePath)

	for _, file := range reader.File {
		// Skip directories and check for regular files only
		if file.FileInfo().IsDir() {
			continue
		}

		// Skip symbolic links (zip files can contain symlinks via mode bits)
		if file.Mode()&os.ModeSymlink != 0 {
			continue
		}

		// Validate file size to prevent decompression bombs
		if file.UncompressedSize64 > maxBinarySize {
			return "", fmt.Errorf("file %s exceeds maximum allowed size (%d bytes)", file.Name, maxBinarySize)
		}

		// Use path.Base for zip entries (always uses forward slashes)
		// This prevents path traversal attacks like "../../etc/passwd"
		entryName := path.Base(file.Name)

		// Validate entry name has no path traversal
		if strings.Contains(entryName, "..") || strings.ContainsAny(entryName, `/\`) {
			continue
		}

		// Check if this is the binary we want
		if !i.isBinaryMatch(entryName, binaryName) {
			continue
		}

		// Open the file in the archive
		rc, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("failed to open file in zip: %w", err)
		}

		// Extract the binary with size limit
		extractedPath := filepath.Join(tempDir, "new-"+binaryName)
		limitedReader := io.LimitReader(rc, maxBinarySize)
		if err := i.extractToFile(extractedPath, limitedReader, file.Mode()); err != nil {
			_ = rc.Close()
			return "", err
		}
		_ = rc.Close()

		return extractedPath, nil
	}

	return "", fmt.Errorf("%w: zip", errBinaryNotInArchive)
}

// isBinaryMatch checks if the entry name matches the expected binary.
//
// The comparison is on whole names, not prefixes: an archive carries
// zzrouter, zzrouter-node and zzrouter-launcher side by side, and every
// one of them is prefixed by the name of another, so a prefix test hands
// the client binary whichever of the three the archive lists first.
func (i *Installer) isBinaryMatch(entryName, binaryName string) bool {
	// Normalize for comparison (case-insensitive on Windows)
	if runtime.GOOS == "windows" {
		entryName = strings.ToLower(entryName)
		binaryName = strings.ToLower(binaryName)
	}

	return archiveBinaryName(entryName) == strings.TrimSuffix(binaryName, ".exe")
}

// archiveBinaryName strips the extension and platform suffix a release
// archive adds, turning "zzrouter-node-linux-amd64" into "zzrouter-node".
func archiveBinaryName(entryName string) string {
	name := strings.TrimSuffix(entryName, ".exe")
	for _, goos := range releaseOSNames {
		if idx := strings.Index(name, "-"+goos+"-"); idx > 0 {
			return name[:idx]
		}
	}
	return name
}

// launcherSibling returns where zzrouter-launcher has to sit for the
// node binary at currentExe, or "" when the binary being updated is not
// the node.
//
// The node locates its launcher next to its own executable and rejects
// it when the hash does not match the one embedded at build time
// (pkg/prov_apps/process.findLauncherBinary). Replacing the node without
// its launcher therefore costs every provider process its PID tracking
// and signal forwarding — silently, because a launcher that fails
// verification is skipped rather than reported.
func launcherSibling(currentExe string) string {
	launcher, node := launcherBinaryName, nodeBinaryName
	base := filepath.Base(currentExe)

	isNode := base == node
	if runtime.GOOS == "windows" {
		launcher += ".exe"
		node += ".exe"
		isNode = strings.EqualFold(base, node)
	}
	if !isNode {
		return ""
	}
	return filepath.Join(filepath.Dir(currentExe), launcher)
}

// extractToFile extracts content from a reader to a file
func (i *Installer) extractToFile(destPath string, reader io.Reader, mode os.FileMode) error {
	// Ensure mode is executable
	if mode == 0 {
		mode = 0750
	}

	outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}

	_, err = io.Copy(outFile, reader)
	closeErr := outFile.Close()

	if err != nil {
		return fmt.Errorf("failed to extract binary: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to close extracted file: %w", closeErr)
	}

	return nil
}

// backupStampLayout is the suffix createBackup appends to a binary name;
// listBackups matches on it so one binary's backups cannot be claimed by
// another whose name is a prefix of it.
//
// Sub-second resolution is load-bearing: Rollback copies the current
// binary aside before restoring, and at second resolution that copy
// lands on the name of the backup it is about to restore — a rollback
// inside the same second as the install restored the new binary over
// itself.
const backupStampLayout = "20060102-150405.000000000"

// backupStampLayouts are the stamps listBackups recognises, newest
// format first. The second-resolution one is what earlier versions
// wrote; it stays readable so an upgraded node can still roll back to
// what its predecessor saved.
var backupStampLayouts = []string{backupStampLayout, "20060102-150405"}

// isBackupStamp reports whether s is the timestamp createBackup appends.
func isBackupStamp(s string) bool {
	for _, layout := range backupStampLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// createBackup creates a backup of the current binary under a fresh
// timestamp.
func (i *Installer) createBackup(currentExe string) (string, error) {
	return i.createBackupAt(currentExe, utils.Now().Format(backupStampLayout))
}

// createBackupAt backs up under a caller-chosen timestamp.
//
// One install stamps the node and the launcher identically on purpose:
// that shared stamp is what lets a later rollback tell which launcher
// backup belongs to which node backup, rather than assuming the newest
// of each go together. They do not when a release ships one binary and
// not the other.
func (i *Installer) createBackupAt(currentExe, timestamp string) (string, error) {
	backupName := fmt.Sprintf("%s-%s", filepath.Base(currentExe), timestamp)
	backupPath := filepath.Join(i.backupDir, backupName)

	// Copy current binary to backup location
	if err := copyFileWithSync(currentExe, backupPath); err != nil {
		return "", fmt.Errorf("failed to create backup: %w", err)
	}

	return backupPath, nil
}

// replaceBinary replaces the current binary with the new one
func (i *Installer) replaceBinary(currentExe, newExe string) error {
	if runtime.GOOS == "windows" {
		return i.replaceBinaryWindows(currentExe, newExe)
	}
	return i.replaceBinaryUnix(currentExe, newExe)
}

// replaceBinaryUnix uses atomic rename on Unix systems
func (i *Installer) replaceBinaryUnix(currentExe, newExe string) error {
	// On Unix, we can use atomic rename
	// But the running process might still have the old file open
	// The common pattern is to:
	// 1. Rename old binary to .old
	// 2. Rename new binary to target
	// 3. Remove .old

	oldPath := currentExe + ".old"

	// Remove any existing .old file
	_ = os.Remove(oldPath)

	// Rename current to .old
	if err := os.Rename(currentExe, oldPath); err != nil {
		return fmt.Errorf("failed to rename current binary: %w", err)
	}

	// Rename new to current
	if err := os.Rename(newExe, currentExe); err != nil {
		// Try to restore
		_ = os.Rename(oldPath, currentExe)
		return fmt.Errorf("failed to rename new binary: %w", err)
	}

	// Remove old binary (may fail if still in use, that's OK)
	_ = os.Remove(oldPath)

	return nil
}

// replaceBinaryWindows handles binary replacement on Windows
// Windows doesn't allow replacing a running executable, so we use a different approach
func (i *Installer) replaceBinaryWindows(currentExe, newExe string) error {
	// On Windows, the typical approach is:
	// 1. Move current exe to .old (Windows allows renaming a running exe)
	// 2. Copy new exe to target location
	// 3. Schedule .old deletion on reboot (optional)

	oldPath := currentExe + ".old"

	// Remove any existing .old file (with retry for Windows)
	for range 3 {
		if err := os.Remove(oldPath); err == nil || os.IsNotExist(err) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Rename current to .old (this works on Windows even for running exe)
	if err := os.Rename(currentExe, oldPath); err != nil {
		return fmt.Errorf("failed to rename current binary: %w", err)
	}

	// Copy new binary to target (can't use rename across volumes on Windows)
	if err := copyFileWithSync(newExe, currentExe); err != nil {
		// Try to restore with retry
		for range 3 {
			if renameErr := os.Rename(oldPath, currentExe); renameErr == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("failed to copy new binary: %w", err)
	}

	// Remove temp file
	_ = os.Remove(newExe)

	// Note: .old file might not be deletable until reboot on Windows
	// if the service is still running with the old exe handle

	return nil
}

// restoreBackup restores from a backup
func (i *Installer) restoreBackup(backupPath, targetPath string) error {
	return copyFileWithSync(backupPath, targetPath)
}

// Rollback restores the most recent backup
func (i *Installer) Rollback() (*InstallResult, error) {
	result := &InstallResult{
		Success: false,
	}

	// Get current executable path
	currentExe, err := i.getCurrentExecutable()
	if err != nil {
		return nil, err
	}

	if l := detectLayout(currentExe); l != nil {
		return i.rollbackManaged(l)
	}

	result.NewPath = currentExe

	// Find most recent backup
	backups, err := i.listBackups(filepath.Base(currentExe))
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	if len(backups) == 0 {
		return nil, ErrNoBackupsAvailable
	}

	// Use most recent backup
	backupPath := backups[0]
	result.BackupPath = backupPath

	// Set the current binary aside, outside the backup directory. Inside
	// it, this copy would be the newest entry and a second rollback
	// would restore the very binary this one is rolling away from.
	if err := i.setAside(currentExe); err != nil {
		return nil, fmt.Errorf("failed to backup current before rollback: %w", err)
	}

	// Restore from backup
	if err := i.restoreBackup(backupPath, currentExe); err != nil {
		return nil, fmt.Errorf("failed to restore from backup: %w", err)
	}

	// Roll the launcher back with it. Restoring the node alone would
	// pair an old binary with the launcher a newer release installed,
	// which is exactly the hash mismatch the pairing exists to avoid.
	if launcherTarget := launcherSibling(currentExe); launcherTarget != "" {
		restored, err := i.rollbackLauncher(launcherTarget, backupPath, filepath.Base(currentExe))
		if err != nil {
			return nil, err
		}
		if restored {
			result.LauncherPath = launcherTarget
		}
	}

	// Consume the backup that was just restored, so rolling back twice
	// walks two versions back instead of restoring the same one again.
	// Nothing is lost: its contents are now the installed binary.
	if err := os.Remove(backupPath); err != nil {
		slog.Warn("restored from a backup that could not then be removed; a second rollback will restore it again",
			"backup", backupPath, "err", err)
	}

	result.Success = true
	return result, nil
}

// rollbackLauncher restores the launcher saved by the same install as
// nodeBackup, and reports whether it restored anything.
//
// Pairing is by the shared timestamp rather than "the newest launcher
// backup". Those are not the same thing: a release that ships no
// launcher leaves the previous one in place and creates no backup, so
// the newest launcher backup can belong to an older node than the one
// being restored. Putting that pair together produces exactly the hash
// mismatch this function exists to prevent.
func (i *Installer) rollbackLauncher(launcherTarget, nodeBackup, nodeName string) (bool, error) {
	stamp, ok := backupStamp(filepath.Base(nodeBackup), nodeName)
	if !ok {
		return false, nil
	}

	paired := filepath.Join(i.backupDir, filepath.Base(launcherTarget)+"-"+stamp)
	if _, err := os.Stat(paired); err != nil {
		// That install never replaced the launcher, so what is on disk
		// already belongs with the node being restored.
		return false, nil //nolint:nilerr // a missing pair is the common case, not a failure
	}

	if err := i.setAside(launcherTarget); err != nil {
		slog.Warn("failed to set the launcher aside before rollback", "err", err)
	}
	if err := i.restoreBackup(paired, launcherTarget); err != nil {
		return false, fmt.Errorf("failed to restore launcher from backup: %w", err)
	}
	// Consumed with its node backup, so the next rollback pairs against
	// the install before this one.
	if err := os.Remove(paired); err != nil {
		slog.Warn("could not remove the launcher backup just restored", "backup", paired, "err", err)
	}
	return true, nil
}

// backupStamp pulls the timestamp off a backup filename by removing the
// binary name it was appended to.
//
// Splitting on the last dash would be wrong twice over: the binary name
// contains one ("zzrouter-node") and so does the stamp itself
// ("20260101-000000.000000000").
func backupStamp(backupName, binaryName string) (string, bool) {
	stamp, ok := strings.CutPrefix(backupName, binaryName+"-")
	if !ok || !isBackupStamp(stamp) {
		return "", false
	}
	return stamp, true
}

// rolledBackDirName holds binaries a rollback displaced. A subdirectory
// keeps them off listBackups, which skips directories, so they are
// recoverable by hand without ever being chosen automatically.
const rolledBackDirName = "rolled-back"

// setAside preserves a binary a rollback is about to overwrite.
func (i *Installer) setAside(binaryPath string) error {
	dir := filepath.Join(i.backupDir, rolledBackDirName)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	name := fmt.Sprintf("%s-%s", filepath.Base(binaryPath), utils.Now().Format(backupStampLayout))
	return copyFileWithSync(binaryPath, filepath.Join(dir, name))
}

// listBackups lists all backups for a binary, sorted by date (most recent first)
func (i *Installer) listBackups(binaryName string) ([]string, error) {
	entries, err := os.ReadDir(i.backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var backups []string
	prefix := binaryName + "-"

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// The remainder has to be the timestamp createBackup appended,
		// or "zzrouter" claims every zzrouter-node and
		// zzrouter-launcher backup sharing the directory.
		stamp, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok || !isBackupStamp(stamp) {
			continue
		}
		backups = append(backups, filepath.Join(i.backupDir, entry.Name()))
	}

	// Sort by name descending (timestamp is in the name, so this gives most recent first)
	sort.Sort(sort.Reverse(sort.StringSlice(backups)))

	return backups, nil
}

// cleanupOldBackups removes old backups beyond the keep limit, for the
// binary and for the launcher installed alongside it.
func (i *Installer) cleanupOldBackups() error {
	// Get current executable to determine binary name
	currentExe, err := os.Executable()
	if err != nil {
		return err
	}

	names := []string{filepath.Base(currentExe)}
	if launcherTarget := launcherSibling(currentExe); launcherTarget != "" {
		names = append(names, filepath.Base(launcherTarget))
	}

	for _, name := range names {
		backups, err := i.listBackups(name)
		if err != nil {
			return err
		}

		// Keep only the specified number of backups
		if len(backups) <= i.keepPreviousVersions {
			continue
		}

		// Remove older backups
		for _, backup := range backups[i.keepPreviousVersions:] {
			if err := os.Remove(backup); err != nil {
				slog.Warn("failed to remove old backup", "backup", backup, "err", err)
			}
		}
	}

	return nil
}

// ListBackups returns available backups for the current binary
func (i *Installer) ListBackups() ([]string, error) {
	currentExe, err := os.Executable()
	if err != nil {
		return nil, err
	}

	binaryName := filepath.Base(currentExe)
	return i.listBackups(binaryName)
}

// copyFileWithSync copies a file from src to dst and syncs to disk
func copyFileWithSync(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source: %w", err)
	}
	defer func() { _ = srcFile.Close() }()

	// Get source file info for permissions
	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat source: %w", err)
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		_ = dstFile.Close()
		return fmt.Errorf("failed to copy content: %w", err)
	}

	// Sync to ensure data is written to disk
	if err := dstFile.Sync(); err != nil {
		_ = dstFile.Close()
		return fmt.Errorf("failed to sync to disk: %w", err)
	}

	if err := dstFile.Close(); err != nil {
		return fmt.Errorf("failed to close destination: %w", err)
	}

	return nil
}
