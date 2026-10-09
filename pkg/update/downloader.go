package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/download"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/utils"
	"github.com/stperic/zzrouter/pkg/version"
)

// ProgressCallback is called during download to report progress
type ProgressCallback func(downloaded, total int64, percent int)

// Downloader handles downloading update assets
type Downloader struct {
	httpClient   *http.Client
	tempDir      string
	callback     ProgressCallback
	allowedHosts []string // Configurable allowed hosts (for testing)

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
		tempDir: tempDir,
	}
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

func (d *Downloader) Download(ctx context.Context, asset *ReleaseAsset) (*DownloadResult, error) {
	if asset == nil {
		return nil, fmt.Errorf("asset is nil")
	}
	if err := d.validateURL(asset.DownloadURL); err != nil {
		return nil, fmt.Errorf("invalid download URL: %w", err)
	}
	if err := os.MkdirAll(d.tempDir, 0o750); err != nil {
		return nil, err
	}
	finalPath := filepath.Join(d.tempDir, asset.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("zzRouter/%s", version.Current.String()))
	start := utils.Now()
	result, err := download.Fetch(d.httpClient, req, finalPath, download.Options{
		Hash: true, Mode: 0o640,
		Progress: func(done, total int64) {
			if total <= 0 {
				total = asset.Size
			}
			if d.callback != nil && total > 0 {
				d.callback(done, total, int(done*100/total))
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return &DownloadResult{
		FilePath: finalPath, Checksum: result.SHA256, Size: result.Size,
		Duration: time.Since(start), Resumed: result.ResumedFrom > 0, ResumedFromBytes: result.ResumedFrom,
	}, nil
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
