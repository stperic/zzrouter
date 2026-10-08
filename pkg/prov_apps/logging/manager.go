package logging

import (
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ManagerConfig contains configuration for log management.
type ManagerConfig struct {
	BaseDir     string        // Root directory for logs
	MaxFileSize int64         // Max log size before rotation (default: 100MB)
	MaxAge      time.Duration // Max age before deletion (default: 7 days)
	Compression bool          // Gzip compress rotated logs (default: true)
	MaxBackups  int           // Max rotated files to keep (default: 3)
}

// DefaultManagerConfig returns defaults.
func DefaultManagerConfig(baseDir string) ManagerConfig {
	return ManagerConfig{
		BaseDir:     baseDir,
		MaxFileSize: 100 * 1024 * 1024,
		MaxAge:      constants.LogMaxAge,
		Compression: true,
		MaxBackups:  3,
	}
}

// Manager manages log file lifecycle: creation, rotation, cleanup.
type Manager struct {
	config ManagerConfig
	mu     sync.RWMutex
}

// NewManager creates a log manager and performs startup cleanup.
func NewManager(config ManagerConfig) *Manager {
	m := &Manager{config: config}
	if err := m.cleanupOnStartup(); err != nil {
		slog.Warn("startup log cleanup failed", "error", err)
	}
	return m
}

// GetBaseDir returns the log directory path.
func (m *Manager) GetBaseDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.BaseDir
}

// CreateLogFile creates or opens a log file for an instance.
// Returns the file handle and path. Automatically rotates if oversized.
func (m *Manager) CreateLogFile(instanceID, provider, model string) (*os.File, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.config.BaseDir, 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create log directory: %w", err)
	}

	fileName := sanitizeForFilename(instanceID) + ".log"
	filePath := filepath.Join(m.config.BaseDir, fileName)

	// Rotate if oversized
	if info, err := os.Stat(filePath); err == nil && info.Size() > m.config.MaxFileSize {
		if err := m.rotateLogFileLocked(filePath); err != nil {
			slog.Warn("failed to rotate oversized log file", "path", filePath, "error", err)
		}
	}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create log file: %w", err)
	}

	return f, filePath, nil
}

// GetLogPath returns the log file path for an instance ID.
func (m *Manager) GetLogPath(instanceID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.config.BaseDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), instanceID) && strings.HasSuffix(entry.Name(), ".log") {
			return filepath.Join(m.config.BaseDir, entry.Name())
		}
	}
	return ""
}

// CheckRotation rotates a log file if it exceeds MaxFileSize.
func (m *Manager) CheckRotation(logPath string) error {
	info, err := os.Stat(logPath)
	if err != nil {
		return nil
	}
	if info.Size() >= m.config.MaxFileSize {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.rotateLogFileLocked(logPath)
	}
	return nil
}

// RotateOversizedCopyTruncate scans the logs directory for active
// provider-instance log files that exceed MaxFileSize and rotates them
// in place using the copy-truncate pattern: the contents are copied to a
// timestamped sibling (then gzipped per policy) and the original file is
// truncated to zero length. The child process's inherited O_APPEND fd
// survives the truncation because the inode is unchanged — subsequent
// writes seek to the (now-zero) end of file.
//
// Trade-off vs. rename-rotation: a small window between "copy complete"
// and "truncate" can lose lines written by a very chatty provider.
// Acceptable for debug/diagnostic streams; the alternative (rename) is
// useless for a live child because the child's fd follows the renamed
// file and the active log is forever mis-named.
//
// Files matching any excludePrefix (e.g. "zzrouter-") are skipped —
// those logs are owned by a different rotation scheme (lumberjack) and
// must not be touched by this janitor.
func (m *Manager) RotateOversizedCopyTruncate(excludePrefixes ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.config.BaseDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}
		if hasAnyPrefix(name, excludePrefixes) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() < m.config.MaxFileSize {
			continue
		}
		logPath := filepath.Join(m.config.BaseDir, name)
		if err := m.copyTruncateLocked(logPath); err != nil {
			slog.Warn("copy-truncate rotation failed", "path", logPath, "error", err)
		}
	}
	return nil
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// copyTruncateLocked performs the copy-truncate rotation on a single file.
// Caller must hold m.mu. On any error returned from the copy phase we
// remove the partial sibling so the next pass starts clean.
func (m *Manager) copyTruncateLocked(logPath string) (err error) {
	basePath := strings.TrimSuffix(logPath, ".log")
	rotNum := m.findNextRotation(basePath)
	rotatedPath := fmt.Sprintf("%s.log.%d", basePath, rotNum)

	// Copy (not rename) — preserves the original inode so the child's
	// inherited fd stays valid.
	src, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(rotatedPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	// Propagate dst.Close errors (final flush) via named return unless we
	// already have a prior error. Also remove the partial rotated file
	// on any failure before truncation.
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(rotatedPath)
		}
	}()

	if _, cerr := io.Copy(dst, src); cerr != nil {
		return cerr
	}

	// Truncate the original. Child's O_APPEND fd continues writing at
	// end-of-file — which is now 0 — so no sparse-hole behavior.
	if err := os.Truncate(logPath, 0); err != nil {
		return err
	}

	if m.config.Compression {
		if cerr := m.compressFile(rotatedPath); cerr != nil {
			slog.Warn("failed to compress rotated log", "path", rotatedPath, "error", cerr)
		}
	}
	if cerr := m.cleanupOldRotations(basePath); cerr != nil {
		slog.Warn("failed to cleanup old log rotations", "basePath", basePath, "error", cerr)
	}
	return nil
}

// CleanupOldLogs removes log files older than MaxAge.
func (m *Manager) CleanupOldLogs() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.config.BaseDir)
	if err != nil {
		return err
	}

	cutoff := utils.Now().Add(-m.config.MaxAge)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".log") && !strings.HasSuffix(name, ".log.gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(m.config.BaseDir, name)); err != nil {
				slog.Warn("failed to remove expired log file", "name", name, "error", err)
			}
		}
	}
	return nil
}

// LogFileInfo describes a log file.
type LogFileInfo struct {
	Path         string    `json:"path"`
	InstanceID   string    `json:"instance_id"`
	Size         int64     `json:"size"`
	ModTime      time.Time `json:"mod_time"`
	IsCompressed bool      `json:"is_compressed"`
}

// ListLogFiles returns info about all log files, newest first.
func (m *Manager) ListLogFiles() ([]LogFileInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.config.BaseDir)
	if err != nil {
		return nil, err
	}

	var files []LogFileInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isLog := strings.HasSuffix(name, ".log")
		isGz := strings.HasSuffix(name, ".gz")
		if !isLog && !isGz {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// Extract instance ID from filename
		instanceID := strings.TrimSuffix(name, ".log")
		instanceID = strings.TrimSuffix(instanceID, ".gz")
		// Remove rotation suffixes
		if idx := strings.Index(instanceID, ".log."); idx > 0 {
			instanceID = instanceID[:idx]
		}

		files = append(files, LogFileInfo{
			Path:         filepath.Join(m.config.BaseDir, name),
			InstanceID:   instanceID,
			Size:         info.Size(),
			ModTime:      info.ModTime(),
			IsCompressed: isGz,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.After(files[j].ModTime)
	})
	return files, nil
}

// --- internal ---

func (m *Manager) cleanupOnStartup() error {
	if err := os.MkdirAll(m.config.BaseDir, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(m.config.BaseDir)
	if err != nil {
		return err
	}

	now := utils.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		filePath := filepath.Join(m.config.BaseDir, entry.Name())

		// Delete old files
		if m.config.MaxAge > 0 && now.Sub(info.ModTime()) > m.config.MaxAge {
			if err := os.Remove(filePath); err != nil {
				slog.Warn("failed to remove old log file", "path", filePath, "error", err)
			}
			continue
		}

		// Compress uncompressed rotated files
		name := entry.Name()
		if m.config.Compression && strings.Contains(name, ".log.") && !strings.HasSuffix(name, ".gz") && info.Size() > 1024 {
			if err := m.compressFile(filePath); err != nil {
				slog.Warn("failed to compress rotated log file", "path", filePath, "error", err)
			}
		}
	}
	return nil
}

func (m *Manager) rotateLogFileLocked(logPath string) error {
	info, err := os.Stat(logPath)
	if err != nil || info.Size() < m.config.MaxFileSize {
		return nil //nolint:nilerr // stat failure (file missing) or under-threshold: rotation is a no-op
	}

	basePath := strings.TrimSuffix(logPath, ".log")
	rotNum := m.findNextRotation(basePath)
	rotatedPath := fmt.Sprintf("%s.log.%d", basePath, rotNum)

	if err := os.Rename(logPath, rotatedPath); err != nil {
		return err
	}

	if m.config.Compression {
		if err := m.compressFile(rotatedPath); err != nil {
			slog.Warn("failed to compress rotated log", "path", rotatedPath, "error", err)
		}
	}
	if err := m.cleanupOldRotations(basePath); err != nil {
		slog.Warn("failed to cleanup old log rotations", "basePath", basePath, "error", err)
	}
	return nil
}

func (m *Manager) findNextRotation(basePath string) int {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 1
	}

	maxNum := 0
	prefix := base + ".log."
	for _, entry := range entries {
		if after, ok := strings.CutPrefix(entry.Name(), prefix); ok {
			suffix := strings.TrimSuffix(after, ".gz")
			var num int
			if _, err := fmt.Sscanf(suffix, "%d", &num); err == nil && num > maxNum {
				maxNum = num
			}
		}
	}
	return maxNum + 1
}

func (m *Manager) compressFile(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(path + ".gz")
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		_ = dst.Close()
		return err
	}
	// Flush to disk before removing the source file to prevent data loss
	// if the system crashes between remove and deferred close.
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}

func (m *Manager) cleanupOldRotations(basePath string) error {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	prefix := base + ".log."
	var rotated []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			rotated = append(rotated, filepath.Join(dir, entry.Name()))
		}
	}

	sort.Slice(rotated, func(i, j int) bool {
		fi, errI := os.Stat(rotated[i])
		fj, errJ := os.Stat(rotated[j])
		if errI != nil || errJ != nil {
			return errI == nil // files that stat successfully sort first
		}
		return fi.ModTime().Before(fj.ModTime())
	})

	if len(rotated) > m.config.MaxBackups {
		for _, f := range rotated[:len(rotated)-m.config.MaxBackups] {
			if err := os.Remove(f); err != nil {
				slog.Warn("failed to remove old log rotation", "path", f, "error", err)
			}
		}
	}
	return nil
}

func sanitizeForFilename(s string) string {
	replacements := map[string]string{
		"/": "-", "\\": "-", ":": "-", "*": "-", "?": "-",
		"\"": "-", "<": "-", ">": "-", "|": "-", " ": "-",
	}
	if runtime.GOOS == "windows" {
		s = strings.TrimRight(s, ". ")
	}
	for old, new := range replacements {
		s = strings.ReplaceAll(s, old, new)
	}
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")

	if runtime.GOOS == "windows" {
		upper := strings.ToUpper(s)
		reserved := []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4",
			"COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4",
			"LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}
		if slices.Contains(reserved, upper) {
			s = "_" + s
		}
	}
	if s == "" {
		s = "unnamed"
	}
	return s
}
