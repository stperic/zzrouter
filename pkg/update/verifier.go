package update

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/stperic/zzinstall/artifact"
	"github.com/stperic/zzrouter/pkg/security"
)

func (v *Verifier) verifyRelease(ctx context.Context, d *Downloader, release *ReleaseInfo, download *DownloadResult) error {
	if _, err := d.DownloadChecksums(ctx, release.asset("checksums.txt")); err != nil {
		return fmt.Errorf("checksums download failed: %w", err)
	}
	bundlePath, err := d.DownloadBundle(ctx, release.asset("checksums.txt.sigstore.json"))
	if err != nil {
		return fmt.Errorf("signature bundle download failed: %w", err)
	}
	result, err := v.Verify(download, bundlePath, release.Version.String())
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	if !result.Valid {
		return fmt.Errorf("verification failed: %s", result.Error)
	}
	return nil
}

// Verifier authenticates release checksums before accepting their archive digest.
type Verifier struct {
	workflow        string
	issuer          string
	trustedMaterial root.TrustedMaterial
}

// NewVerifier fixes the publisher authority independently of release metadata.
func NewVerifier(owner, repo string) *Verifier {
	return &Verifier{workflow: fmt.Sprintf("https://github.com/%s/%s/.github/workflows/release.yaml", owner, repo), issuer: "https://token.actions.githubusercontent.com"}
}

// VerifyChecksum verifies a file's SHA256 checksum.
func (v *Verifier) VerifyChecksum(filePath, expectedHash string) error {
	return security.VerifySHA256(filePath, expectedHash)
}

// VerifyChecksums verifies a file against an authenticated checksum map.
func (v *Verifier) VerifyChecksums(filePath string, checksums map[string]string) error {
	filename := filepath.Base(filePath)
	expected, ok := checksums[filename]
	if !ok {
		return fmt.Errorf("no checksum found for %s", filename)
	}
	return v.VerifyChecksum(filePath, expected)
}

// VerifySignature requires an offline-verifiable bundle for the exact release tag.
func (v *Verifier) VerifySignature(checksumsPath, bundlePath, releaseVersion string) error {
	content, err := readVerificationFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}
	return v.verifySignedChecksums(content, bundlePath, releaseVersion)
}

func (v *Verifier) verifySignedChecksums(content []byte, bundlePath, releaseVersion string) error {
	return (&artifact.Verifier{Workflow: v.workflow, Issuer: v.issuer, TrustedMaterial: v.trustedMaterial}).VerifySignature(content, bundlePath, releaseVersion)
}
func readVerificationFile(path string) ([]byte, error) { return artifact.ReadMetadata(path) }

// Verify binds the signed checksums, release identity and downloaded archive.
func (v *Verifier) Verify(download *DownloadResult, bundlePath, releaseVersion string) (*VerifyResult, error) {
	result := new(VerifyResult)
	content, err := readVerificationFile(filepath.Join(filepath.Dir(download.FilePath), "checksums.txt"))
	if err == nil {
		err = v.verifySignedChecksums(content, bundlePath, releaseVersion)
	}
	if err != nil {
		result.Error = fmt.Sprintf("signature verification failed: %v", err)
		return result, nil
	}
	result.SignatureValid = true
	checksums, err := parseChecksums(content)
	if err == nil {
		err = v.VerifyChecksums(download.FilePath, checksums)
	}
	if err != nil {
		result.Error = fmt.Sprintf("checksum verification failed: %v", err)
		return result, nil
	}
	result.ChecksumValid = true
	result.Valid = true
	return result, nil
}
