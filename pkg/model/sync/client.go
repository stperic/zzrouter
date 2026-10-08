package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/model/integrity"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Client handles downloading models from other nodes in the cluster over
// the mTLS cluster port. Security: all operations validate checksums, use
// atomic writes, and enforce size limits.
type Client struct {
	httpClient     *http.Client
	maxFileSize    int64         // Maximum file size to download (default: 100GB)
	chunkSize      int64         // Size of chunks for progress reporting (default: 10MB)
	requestTimeout time.Duration // Timeout for manifest/exists requests (default: 30s)
}

// ClientConfig configures the sync client.
type ClientConfig struct {
	// HTTPClient is the mTLS dispatch client built by
	// clusternode.Node.DialClient(RoleCoordinator.OU()). Required —
	// sync only flows over mTLS.
	HTTPClient *http.Client

	MaxFileSize    int64         // Maximum file size (0 = 100GB default)
	ChunkSize      int64         // Chunk size for progress (0 = 10MB default)
	RequestTimeout time.Duration // Timeout for requests (0 = 30s default)
}

// NewClient creates a new sync client. Returns nil when cfg.HTTPClient is
// nil — sync requires the mTLS dispatch client; non-worker callers must
// not invoke sync.
func NewClient(cfg ClientConfig) *Client {
	if cfg.HTTPClient == nil {
		return nil
	}

	maxFileSize := cfg.MaxFileSize
	if maxFileSize == 0 {
		maxFileSize = constants.SyncMaxFileSize // 100GB
	}
	chunkSize := cfg.ChunkSize
	if chunkSize == 0 {
		chunkSize = 10 * constants.BytesPerMB // 10MB
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = constants.HTTPDefaultTimeout
	}

	return &Client{
		httpClient:     cfg.HTTPClient,
		maxFileSize:    maxFileSize,
		chunkSize:      chunkSize,
		requestTimeout: requestTimeout,
	}
}

// Progress reports progress during sync.
type Progress struct {
	File            string  `json:"file"`
	BytesDownloaded int64   `json:"bytes_downloaded"`
	BytesTotal      int64   `json:"bytes_total"`
	Progress        float64 `json:"progress"`
	Speed           int64   `json:"speed"` // bytes per second
	FilesCompleted  int     `json:"files_completed"`
	FilesTotal      int     `json:"files_total"`
}

// ProgressCallback is called with progress updates during sync.
type ProgressCallback func(progress Progress)

// Result represents the result of a sync operation.
type Result struct {
	Success         bool     `json:"success"`
	FilesDownloaded int      `json:"files_downloaded"`
	FilesSkipped    int      `json:"files_skipped"`
	FilesFailed     int      `json:"files_failed"`
	BytesDownloaded int64    `json:"bytes_downloaded"`
	Errors          []string `json:"errors,omitempty"`
}

// FetchManifest fetches the model manifest from a source node.
func (c *Client) FetchManifest(ctx context.Context, sourceURL string, request metadata.DownloadRequest, format string) (*SyncManifest, error) {
	reqURL := fmt.Sprintf("%s/zzrouter/v1/internal/sync/manifest?model=%s&format=%s",
		strings.TrimSuffix(sourceURL, "/"), url.QueryEscape(request.Repo), url.QueryEscape(format))

	reqCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()

	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(reqCtx, "POST", reqURL, bytes.NewReader(body))
	if req != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("model not found on source node: %s/%s", request.Repo, format)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("manifest request failed (status %d): %s", resp.StatusCode, string(body))
	}

	var manifest SyncManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&manifest); err != nil { // 1MB limit
		return nil, fmt.Errorf("failed to parse manifest: %w", err)
	}

	if err := c.validateManifest(&manifest); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	if manifest.Model != request.Repo {
		return nil, fmt.Errorf("source returned a different model")
	}
	if request.Files != nil {
		if len(manifest.Files) != len(request.Files) {
			return nil, fmt.Errorf("source returned a different file set")
		}
		selected := map[string]metadata.DownloadFile{}
		for _, f := range request.Files {
			selected[f.Name] = f
		}
		for _, f := range manifest.Files {
			want, ok := selected[f.Name]
			if !ok || want.Size != f.Size || want.Feature != f.Feature || (want.SHA256 != "" && !strings.EqualFold(want.SHA256, f.SHA256)) {
				return nil, fmt.Errorf("source file differs from plan: %s", f.Name)
			}
			delete(selected, f.Name)
		}
	}

	return &manifest, nil
}

// validateManifest validates the manifest for security issues.
func (c *Client) validateManifest(manifest *SyncManifest) error {
	if len(manifest.Files) == 0 {
		return fmt.Errorf("manifest has no files")
	}

	for _, file := range manifest.Files {
		if strings.Contains(file.Name, "..") {
			return fmt.Errorf("suspicious path in manifest: %s", file.Name)
		}
		if filepath.IsAbs(file.Name) {
			return fmt.Errorf("absolute path in manifest: %s", file.Name)
		}
		if file.Size > c.maxFileSize {
			return fmt.Errorf("file exceeds size limit: %s (%d bytes > %d bytes)",
				file.Name, file.Size, c.maxFileSize)
		}
		if len(file.SHA256) != 64 {
			return fmt.Errorf("invalid checksum format for file: %s", file.Name)
		}
	}

	return nil
}

// SyncModel downloads a model from source node to local storage.
// Security features: atomic writes (temp file + rename), SHA256
// verification after download, size limits enforced, path traversal
// prevention.
func (c *Client) SyncModel(ctx context.Context, sourceURL string, request metadata.DownloadRequest, format string, callback ProgressCallback) (*Result, error) {
	model := request.Repo
	if model == "" || filepath.IsAbs(model) || strings.Contains(model, "..") {
		return nil, fmt.Errorf("invalid model path")
	}
	slog.Info("[sync.Client] Starting sync", "model", model, "format", format, "source", sourceURL)

	manifest, err := c.FetchManifest(ctx, sourceURL, request, format)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}

	slog.Info("[sync.Client] manifest received", "files", len(manifest.Files), "total_size", manifest.TotalSize)

	modelsRoot, err := modelregistry.GetModelsRootDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get models root: %w", err)
	}

	security := NewSecurityFromRoot(modelsRoot)
	destDir, err := security.ValidateWritePath(model)
	if err != nil {
		return nil, err
	}
	release, err := integrity.AcquireMaterialization(ctx, destDir)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create destination directory: %w", err)
	}

	result := &Result{Success: true}

	for i, file := range manifest.Files {
		select {
		case <-ctx.Done():
			result.Success = false
			result.Errors = append(result.Errors, "sync cancelled")
			return result, ctx.Err()
		default:
		}

		destPath, err := security.ValidateWritePath(filepath.Join(model, file.Name))
		if err != nil {
			return result, err
		}

		// Security: destPath must be contained within destDir.
		absDestPath, err := filepath.Abs(destPath)
		if err != nil {
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: failed to resolve path", file.Name))
			continue
		}
		absDestDir, _ := filepath.Abs(destDir)
		if !isWithin(absDestDir, absDestPath) {
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: path traversal detected", file.Name))
			continue
		}

		if !request.Force && c.fileExistsWithChecksum(destPath, file.SHA256) {
			slog.Info("[sync.Client] Skipping file (already exists)", "name", file.Name)
			result.FilesSkipped++
			if callback != nil {
				callback(Progress{
					File:            file.Name,
					BytesDownloaded: file.Size,
					BytesTotal:      file.Size,
					Progress:        100,
					FilesCompleted:  i + 1,
					FilesTotal:      len(manifest.Files),
				})
			}
			continue
		}

		fileDir := filepath.Dir(destPath)
		if err := os.MkdirAll(fileDir, 0755); err != nil {
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: failed to create directory", file.Name))
			continue
		}

		downloaded, err := c.downloadFile(ctx, sourceURL, model, format, file, destPath, func(bytesDownloaded, speed int64) {
			if callback != nil {
				progress := float64(0)
				if file.Size > 0 {
					progress = float64(bytesDownloaded) / float64(file.Size) * 100
				}
				callback(Progress{
					File:            file.Name,
					BytesDownloaded: bytesDownloaded,
					BytesTotal:      file.Size,
					Progress:        progress,
					Speed:           speed,
					FilesCompleted:  i,
					FilesTotal:      len(manifest.Files),
				})
			}
		})

		if err != nil {
			slog.Error("Failed to download sync file", "name", file.Name, "error", err)
			result.FilesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", file.Name, err))
			result.Success = false
			continue
		}

		result.FilesDownloaded++
		result.BytesDownloaded += downloaded

		slog.Info("[sync.Client] file downloaded", "file", file.Name, "bytes", downloaded)

		if callback != nil {
			callback(Progress{
				File:            file.Name,
				BytesDownloaded: file.Size,
				BytesTotal:      file.Size,
				Progress:        100,
				FilesCompleted:  i + 1,
				FilesTotal:      len(manifest.Files),
			})
		}
	}

	if result.FilesFailed > 0 {
		result.Success = false
	}

	if result.Success && len(manifest.Files) > 0 {
		if err := RecordManifest(destDir, model, sourceURL, manifest); err != nil {
			return result, fmt.Errorf("record synced files: %w", err)
		}
	}

	slog.Info("[sync.Client] sync complete", "files_downloaded", result.FilesDownloaded, "files_skipped", result.FilesSkipped, "files_failed", result.FilesFailed)

	return result, nil
}

// fileExistsWithChecksum checks if a file exists and has the expected checksum.
func (c *Client) fileExistsWithChecksum(path, expectedSHA256 string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}

	checksum, err := ComputeSHA256(path)
	if err != nil {
		return false
	}

	return strings.EqualFold(checksum, expectedSHA256)
}

// downloadFile downloads a single file with atomic write and checksum
// verification.
func (c *Client) downloadFile(
	ctx context.Context,
	sourceURL, model, format string,
	file FileChecksum,
	destPath string,
	progressCallback func(bytesDownloaded, speed int64),
) (int64, error) {
	reqURL := fmt.Sprintf("%s/zzrouter/v1/internal/sync/file?model=%s&format=%s&file=%s",
		strings.TrimSuffix(sourceURL, "/"), url.QueryEscape(model), url.QueryEscape(format), url.QueryEscape(file.Name))

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("download failed (status %d): %s", resp.StatusCode, string(body))
	}

	tempPath := destPath + ".tmp." + shortID()
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create temp file: %w", err)
	}

	cleanupTemp := true
	defer func() {
		_ = tempFile.Close()
		if cleanupTemp {
			_ = os.Remove(tempPath)
		}
	}()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(tempFile, hasher)

	// +1 to detect oversized files.
	limitedReader := io.LimitReader(resp.Body, c.maxFileSize+1)

	var bytesDownloaded int64
	startTime := utils.Now()
	lastProgressTime := startTime
	lastProgressBytes := int64(0)
	buf := make([]byte, 32*1024) // 32KB buffer

	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		n, readErr := limitedReader.Read(buf)
		if n > 0 {
			_, writeErr := multiWriter.Write(buf[:n])
			if writeErr != nil {
				return 0, fmt.Errorf("write error: %w", writeErr)
			}
			bytesDownloaded += int64(n)

			if bytesDownloaded > c.maxFileSize {
				return 0, fmt.Errorf("file exceeds maximum size limit (%d bytes)", c.maxFileSize)
			}

			now := utils.Now()
			if now.Sub(lastProgressTime) >= time.Second && progressCallback != nil {
				elapsed := now.Sub(lastProgressTime).Seconds()
				speed := int64(float64(bytesDownloaded-lastProgressBytes) / elapsed)
				progressCallback(bytesDownloaded, speed)
				lastProgressTime = now
				lastProgressBytes = bytesDownloaded
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return 0, fmt.Errorf("read error: %w", readErr)
		}
	}

	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(actualChecksum, file.SHA256) {
		return 0, fmt.Errorf("checksum mismatch: expected %s, got %s", file.SHA256, actualChecksum)
	}

	if bytesDownloaded != file.Size {
		return 0, fmt.Errorf("size mismatch: expected %d, got %d", file.Size, bytesDownloaded)
	}

	if err := tempFile.Sync(); err != nil {
		return 0, fmt.Errorf("failed to sync file: %w", err)
	}
	_ = tempFile.Close()

	if err := os.Rename(tempPath, destPath); err != nil {
		return 0, fmt.Errorf("failed to rename temp file: %w", err)
	}

	cleanupTemp = false
	return bytesDownloaded, nil
}

// shortID returns an 8-char hex ID suitable for temp filename disambiguation.
// crypto/rand failure falls back to a nanosecond-derived value because
// cosmetic uniqueness is acceptable for temp files.
func shortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", utils.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(b)
}
