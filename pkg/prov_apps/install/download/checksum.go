package download

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/security"
)

// checksumFileMaxBytes caps the SHA256SUMS-style file we'll parse. Real
// upstream files are <100KB; the cap stops a malicious mirror from
// streaming forever.
const checksumFileMaxBytes = 1 << 20

// VerifyChecksumFromURL downloads a SHA256SUMS-style file (one
// "<hash>  <filename>" per line) and verifies localPath matches the
// entry for archiveName. When the checksum file is unavailable
// (404, 5xx) or the archive isn't listed, the call returns nil and
// logs a warning — degraded mode mirrors the llama.cpp install path.
// A real hash mismatch is a hard error.
//
// checksumsURL must already be host-validated by the caller; this
// helper does NOT call ValidateInstallHost (the per-installer fetch
// path that produced archivePath did, and the checksums file lives
// at the same release).
func VerifyChecksumFromURL(ctx context.Context, checksumsURL, archivePath, archiveName string) error {
	client := NewClient(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, "GET", checksumsURL, nil)
	if err != nil {
		return fmt.Errorf("create checksum request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("checksum file not available, skipping verification",
			"url", checksumsURL, "status", resp.StatusCode, "archive", archiveName)
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, checksumFileMaxBytes))
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	expected := lookupChecksum(string(body), archiveName)
	if expected == "" {
		slog.Warn("archive not listed in checksum file, skipping verification",
			"archive", archiveName, "url", checksumsURL)
		return nil
	}

	if err := security.VerifySHA256(archivePath, expected); err != nil {
		return fmt.Errorf("archive integrity check FAILED: %w", err)
	}
	slog.Info("archive checksum verified", "archive", archiveName, "sha256", expected)
	return nil
}

// lookupChecksum scans a SHA256SUMS-style document for a line whose
// second field equals archiveName and returns the first field
// (hex-encoded hash). Returns "" when not found. Tolerates leading
// "*" markers that some hashers prefix to binary-mode filenames.
func lookupChecksum(doc, archiveName string) string {
	for _, line := range strings.Split(doc, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Compare basenames. sha256sum writes "*name" in binary mode, and
		// ollama publishes every entry as "./name" — matched literally, the
		// second form never hit, so the lookup silently returned "" and the
		// caller skipped verification for every ollama artifact ever shipped.
		if path.Base(strings.TrimPrefix(fields[1], "*")) == path.Base(archiveName) {
			return fields[0]
		}
	}
	return ""
}
