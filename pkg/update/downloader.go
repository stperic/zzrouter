package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// ProgressCallback is called during download to report progress
type ProgressCallback func(downloaded, total int64, percent int)

// partialFileSuffix is the suffix for incomplete downloads
const partialFileSuffix = ".partial"

// Downloader handles downloading update assets
type Downloader struct {
	httpClient   *http.Client
	tempDir      string
	callback     ProgressCallback
	allowedHosts []string // Configurable allowed hosts (for testing)
	enableResume bool     // Enable download resume support (default: true)

	// allowLoopbackHTTP permits plaintext downloads from loopback
	// addresses. Off unless node.yaml points update.source at a
	// loopback-only feed.
	allowLoopbackHTTP bool
}

// NewDownloader creates a new downloader
func NewDownloader(tempDir string) *Downloader {
	return &Downloader{
		httpClient: &http.Client{
			Timeout: 10 * time.Minute, // Long timeout for large downloads
		},
		tempDir:      tempDir,
		enableResume: true,
	}
}

// SetResumeEnabled enables or disables download resume support
func (d *Downloader) SetResumeEnabled(enabled bool) {
	d.enableResume = enabled
}

// SetProgressCallback sets the callback for download progress updates
func (d *Downloader) SetProgressCallback(callback ProgressCallback) {
	d.callback = callback
}

// SetAllowedHosts sets custom allowed hosts (for testing)
func (d *Downloader) SetAllowedHosts(hosts []string) {
	d.allowedHosts = hosts
}

// SetAllowLoopbackHTTP lifts the HTTPS-only rule for loopback download
// URLs. Callers must gate this on an explicit, operator-set release
// source; see security.ValidateLoopbackDownloadURL.
func (d *Downloader) SetAllowLoopbackHTTP(allow bool) {
	d.allowLoopbackHTTP = allow
}

// defaultAllowedHosts returns the default list of allowed download hosts
var defaultAllowedHosts = []string{
	"github.com",
	"objects.githubusercontent.com",
	"github-releases.githubusercontent.com",
}

// maxMetadataFileSize is the maximum size for checksum, signature, and certificate files
const maxMetadataFileSize = 1024 * 1024 // 1MB

// Download downloads a release asset to the temp directory with optional resume support.
//
//nolint:gocyclo,cyclop // resume-aware download: partial-file detection, range request, checksum, rename are all one-shot steps
func (d *Downloader) Download(ctx context.Context, asset *ReleaseAsset) (*DownloadResult, error) {
	if asset == nil {
		return nil, fmt.Errorf("asset is nil")
	}

	// Validate URL before downloading
	if err := d.validateURL(asset.DownloadURL); err != nil {
		return nil, fmt.Errorf("invalid download URL: %w", err)
	}

	// Ensure temp directory exists
	if err := os.MkdirAll(d.tempDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	finalPath := filepath.Join(d.tempDir, asset.Name)
	partialPath := finalPath + partialFileSuffix

	// Check for existing partial download
	var resumeOffset int64
	var outFile *os.File
	var err error

	if d.enableResume {
		if info, statErr := os.Stat(partialPath); statErr == nil && info.Size() > 0 {
			// Partial file exists, try to resume
			resumeOffset = info.Size()
		}
	}

	// Open file in appropriate mode
	if resumeOffset > 0 {
		outFile, err = os.OpenFile(partialPath, os.O_WRONLY, 0640)
		if err != nil {
			// Fall back to fresh download
			resumeOffset = 0
			outFile, err = os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
		}
	} else {
		outFile, err = os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() { _ = outFile.Close() }()
	// A writable handle supports Windows truncation when a server restarts the transfer.
	if resumeOffset > 0 {
		if _, err := outFile.Seek(resumeOffset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("failed to seek to resume offset: %w", err)
		}
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("zzRouter/%s", version.Current.String()))

	// Add Range header for resume
	if resumeOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeOffset))
	}

	startTime := utils.Now()

	// Execute request
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle response status
	var resumed bool
	var totalSize int64

	switch resp.StatusCode {
	case http.StatusOK:
		// Server sent full file (either doesn't support Range or ignored it)
		if resumeOffset > 0 {
			// Need to start fresh - truncate and seek to beginning
			if err := outFile.Truncate(0); err != nil {
				return nil, fmt.Errorf("failed to truncate partial file: %w", err)
			}
			if _, err := outFile.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("failed to seek to start: %w", err)
			}
			resumeOffset = 0
		}
		totalSize = resp.ContentLength
		if totalSize <= 0 {
			totalSize = asset.Size
		}
	case http.StatusPartialContent:
		// Server supports Range and is sending partial content
		resumed = true
		// Parse Content-Range header: "bytes start-end/total"
		contentRange := resp.Header.Get("Content-Range")
		if contentRange != "" {
			var start, end, total int64
			if _, err := fmt.Sscanf(contentRange, "bytes %d-%d/%d", &start, &end, &total); err == nil {
				totalSize = total
			}
		}
		if totalSize <= 0 {
			totalSize = asset.Size
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// The range was invalid, likely file is complete or changed
		// Start fresh
		if err := outFile.Truncate(0); err != nil {
			return nil, fmt.Errorf("failed to truncate file: %w", err)
		}
		if _, err := outFile.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("failed to seek: %w", err)
		}
		resumeOffset = 0
		// Retry without Range header
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create retry request: %w", err)
		}
		req.Header.Set("User-Agent", fmt.Sprintf("zzRouter/%s", version.Current.String()))
		_ = resp.Body.Close()
		resp, err = d.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to retry download: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
		}
		totalSize = resp.ContentLength
		if totalSize <= 0 {
			totalSize = asset.Size
		}
	default:
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	// For checksum calculation, we need to hash the entire file
	// If resuming, we need to read the existing content first
	hasher := sha256.New()
	if resumed && resumeOffset > 0 {
		// Read existing partial content to include in hash
		partialFile, err := os.Open(partialPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open partial file for hashing: %w", err)
		}
		// Read only up to resumeOffset bytes
		if _, err := io.CopyN(hasher, partialFile, resumeOffset); err != nil && err != io.EOF {
			_ = partialFile.Close()
			return nil, fmt.Errorf("failed to hash existing content: %w", err)
		}
		_ = partialFile.Close()
	}

	multiWriter := io.MultiWriter(outFile, hasher)

	// Download with progress tracking
	downloaded := resumeOffset
	buf := make([]byte, 32*1024) // 32KB buffer

	for {
		select {
		case <-ctx.Done():
			// Keep partial file for resume on context cancellation
			return nil, ctx.Err()
		default:
		}

		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := multiWriter.Write(buf[:n])
			if writeErr != nil {
				return nil, fmt.Errorf("failed to write: %w", writeErr)
			}
			downloaded += int64(n)

			// Report progress
			if d.callback != nil && totalSize > 0 {
				percent := int(float64(downloaded) / float64(totalSize) * 100)
				d.callback(downloaded, totalSize, percent)
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			// On error, keep partial file for resume
			return nil, fmt.Errorf("failed to read: %w", err)
		}
	}

	// Sync to disk before renaming
	if err := outFile.Sync(); err != nil {
		return nil, fmt.Errorf("failed to sync file: %w", err)
	}
	_ = outFile.Close()

	// Rename partial file to final name
	if err := os.Rename(partialPath, finalPath); err != nil {
		return nil, fmt.Errorf("failed to finalize download: %w", err)
	}

	duration := time.Since(startTime)

	return &DownloadResult{
		FilePath:         finalPath,
		Checksum:         hex.EncodeToString(hasher.Sum(nil)),
		Size:             downloaded,
		Duration:         duration,
		Resumed:          resumed,
		ResumedFromBytes: resumeOffset,
	}, nil
}

// GetPartialDownloadSize returns the size of an existing partial download, or 0 if none exists
func (d *Downloader) GetPartialDownloadSize(assetName string) int64 {
	partialPath := filepath.Join(d.tempDir, assetName+partialFileSuffix)
	if info, err := os.Stat(partialPath); err == nil {
		return info.Size()
	}
	return 0
}

// RemovePartialDownload removes a partial download file
func (d *Downloader) RemovePartialDownload(assetName string) error {
	partialPath := filepath.Join(d.tempDir, assetName+partialFileSuffix)
	if err := os.Remove(partialPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DownloadChecksums downloads and parses the checksums file
func (d *Downloader) DownloadChecksums(ctx context.Context, asset *ReleaseAsset) (map[string]string, error) {
	filePath, content, err := d.downloadMetadataFile(ctx, asset, "checksums")
	if err != nil {
		return nil, err
	}
	_ = filePath // saved to disk for signature verification

	return parseChecksums(content)
}

func parseChecksums(content []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	for _, line := range splitLines(string(content)) {
		parts := splitChecksumLine(line)
		if len(parts) < 2 {
			continue
		}
		if _, exists := checksums[parts[1]]; exists {
			return nil, fmt.Errorf("duplicate checksum for %s", parts[1])
		}
		checksums[parts[1]] = parts[0]
	}
	return checksums, nil
}

// DownloadBundle downloads bounded Sigstore verification metadata.
func (d *Downloader) DownloadBundle(ctx context.Context, asset *ReleaseAsset) (string, error) {
	path, _, err := d.downloadMetadataFile(ctx, asset, "signature bundle")
	return path, err
}

// DownloadSignature downloads the signature file
func (d *Downloader) DownloadSignature(ctx context.Context, asset *ReleaseAsset) (string, error) {
	filePath, _, err := d.downloadMetadataFile(ctx, asset, "signature")
	return filePath, err
}

// DownloadCertificate downloads the certificate file
func (d *Downloader) DownloadCertificate(ctx context.Context, asset *ReleaseAsset) (string, error) {
	filePath, _, err := d.downloadMetadataFile(ctx, asset, "certificate")
	return filePath, err
}

// downloadMetadataFile downloads a small metadata file (checksums, signature, certificate),
// saves it to the temp directory, and returns both the path and raw content.
func (d *Downloader) downloadMetadataFile(ctx context.Context, asset *ReleaseAsset, kind string) (string, []byte, error) {
	if asset == nil {
		return "", nil, fmt.Errorf("%s asset is nil", kind)
	}

	if err := d.validateURL(asset.DownloadURL); err != nil {
		return "", nil, fmt.Errorf("invalid %s URL: %w", kind, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("zzRouter/%s", version.Current.String()))

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("failed to download %s: %w", kind, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%s download returned status %d", kind, resp.StatusCode)
	}

	limitedReader := io.LimitReader(resp.Body, maxMetadataFileSize)
	content, err := io.ReadAll(limitedReader)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read %s: %w", kind, err)
	}
	if int64(len(content)) >= maxMetadataFileSize {
		return "", nil, fmt.Errorf("%s file exceeds maximum allowed size (%d bytes)", kind, maxMetadataFileSize)
	}

	filePath := filepath.Join(d.tempDir, asset.Name)
	if err := os.WriteFile(filePath, content, 0640); err != nil {
		return "", nil, fmt.Errorf("failed to save %s file: %w", kind, err)
	}

	return filePath, content, nil
}

// validateURL validates a download URL against the configured or default allowed hosts.
func (d *Downloader) validateURL(downloadURL string) error {
	hosts := d.allowedHosts
	if len(hosts) == 0 {
		hosts = defaultAllowedHosts
	}
	if d.allowLoopbackHTTP {
		return security.ValidateLoopbackDownloadURL(downloadURL, hosts)
	}
	return security.ValidateDownloadURL(downloadURL, hosts)
}

// Cleanup removes downloaded files
func (d *Downloader) Cleanup() error {
	return os.RemoveAll(d.tempDir)
}

// splitLines splits a string into lines, handling both Unix and Windows line endings
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			// Remove trailing \r if present
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	// Handle last line without newline
	if start < len(s) {
		line := s[start:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// splitChecksumLine splits a checksum line (format: "hash  filename" or "hash filename")
func splitChecksumLine(line string) []string {
	var parts []string
	inWord := false
	start := 0

	for i := 0; i < len(line); i++ {
		if line[i] == ' ' || line[i] == '\t' {
			if inWord {
				parts = append(parts, line[start:i])
				inWord = false
			}
		} else {
			if !inWord {
				start = i
				inWord = true
			}
		}
	}

	if inWord {
		parts = append(parts, line[start:])
	}

	return parts
}
