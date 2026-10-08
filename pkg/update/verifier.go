package update

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/stperic/zzrouter/pkg/security"
	"github.com/stperic/zzrouter/pkg/version"
)

// The public trust material was authenticated through Sigstore TUF before embedding.
//
//go:embed sigstore_trusted_root.json
var sigstoreTrustedRoot string

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
	if bundlePath == "" {
		return fmt.Errorf("release requires checksums.txt.sigstore.json; detached signatures are not sufficient")
	}
	parsed, err := version.ParseVersion(releaseVersion)
	if err != nil || parsed.String() != releaseVersion {
		return fmt.Errorf("invalid release version %q", releaseVersion)
	}
	encoded, err := readVerificationFile(bundlePath)
	if err != nil {
		return fmt.Errorf("read signature bundle: %w", err)
	}
	entity := new(bundle.Bundle)
	if err := entity.UnmarshalJSON(encoded); err != nil {
		return fmt.Errorf("decode signature bundle: %w", err)
	}
	if bundleVersion, err := entity.Version(); err != nil || bundleVersion != "v0.3" {
		return fmt.Errorf("release requires Sigstore bundle v0.3")
	}
	material := v.trustedMaterial
	if material == nil {
		material, err = root.NewTrustedRootFromJSON([]byte(sigstoreTrustedRoot))
		if err != nil {
			return fmt.Errorf("load built-in Sigstore trust: %w", err)
		}
	}
	verifier, err := verify.NewVerifier(material, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("initialize Sigstore verification: %w", err)
	}
	identity, err := verify.NewShortCertificateIdentity(v.issuer, "", v.workflow+"@refs/tags/v"+releaseVersion, "")
	if err != nil {
		return fmt.Errorf("initialize release identity: %w", err)
	}
	if _, err = verifier.Verify(entity, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(content)), verify.WithCertificateIdentity(identity))); err != nil {
		return fmt.Errorf("authenticate release checksums: %w", err)
	}
	return nil
}

func readVerificationFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxMetadataFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || int64(len(content)) > maxMetadataFileSize {
		return nil, fmt.Errorf("verification metadata must contain 1 to %d bytes", maxMetadataFileSize)
	}
	return content, nil
}

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
