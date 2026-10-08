package update

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bundleproto "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	commonproto "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	rekorproto "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/tlog"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewVerifier(t *testing.T) {
	v := NewVerifier("stperic", "zzrouter")
	assert.NotNil(t, v)
	assert.Equal(t, "https://github.com/stperic/zzrouter/.github/workflows/release.yaml", v.workflow)
	assert.Equal(t, "https://token.actions.githubusercontent.com", v.issuer)
}

func TestVerifier_VerifyChecksum_Valid(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "testfile")

	// Write test content
	content := []byte("test content for checksum verification")
	err := os.WriteFile(testFile, content, 0640)
	require.NoError(t, err)

	// Compute expected hash
	hasher := sha256.New()
	hasher.Write(content)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	v := NewVerifier("stperic", "zzrouter")

	// Verify should pass
	err = v.VerifyChecksum(testFile, expectedHash)
	assert.NoError(t, err)
}

func TestVerifier_VerifyChecksum_Invalid(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "testfile")

	// Write test content
	err := os.WriteFile(testFile, []byte("test content"), 0640)
	require.NoError(t, err)

	v := NewVerifier("stperic", "zzrouter")

	// Verify should fail with wrong hash
	err = v.VerifyChecksum(testFile, "0000000000000000000000000000000000000000000000000000000000000000")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestVerifier_VerifyChecksum_FileNotFound(t *testing.T) {
	v := NewVerifier("stperic", "zzrouter")

	err := v.VerifyChecksum("/nonexistent/file", "somehash")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open file")
}

func TestVerifier_VerifyChecksum_CaseInsensitive(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "testfile")

	content := []byte("test content")
	err := os.WriteFile(testFile, content, 0640)
	require.NoError(t, err)

	hasher := sha256.New()
	hasher.Write(content)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	v := NewVerifier("stperic", "zzrouter")

	// Lower case should work
	err = v.VerifyChecksum(testFile, expectedHash)
	assert.NoError(t, err)

	// Upper case should work too
	err = v.VerifyChecksum(testFile, strings.ToUpper(expectedHash))
	assert.NoError(t, err)
}

func TestVerifier_VerifyChecksums(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "testfile.tar.gz")

	content := []byte("test content for checksums verification")
	err := os.WriteFile(testFile, content, 0640)
	require.NoError(t, err)

	hasher := sha256.New()
	hasher.Write(content)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	checksums := map[string]string{
		"testfile.tar.gz":  expectedHash,
		"otherfile.tar.gz": "otherhash",
	}

	v := NewVerifier("stperic", "zzrouter")

	err = v.VerifyChecksums(testFile, checksums)
	assert.NoError(t, err)
}

func TestVerifier_VerifyChecksums_NotInMap(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "unknownfile.tar.gz")

	err := os.WriteFile(testFile, []byte("content"), 0640)
	require.NoError(t, err)

	checksums := map[string]string{
		"testfile.tar.gz": "somehash",
	}

	v := NewVerifier("stperic", "zzrouter")

	err = v.VerifyChecksums(testFile, checksums)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no checksum found")
}

func signedUpdateChecksums(t *testing.T, content []byte, releaseVersion string) ([]byte, root.TrustedMaterial) {
	t.Helper()
	return signedUpdateChecksumsFrom(t, content, releaseVersion, "stperic/zzrouter")
}

func signedUpdateChecksumsFrom(t *testing.T, content []byte, releaseVersion, publisher string) ([]byte, root.TrustedMaterial) {
	t.Helper()
	authority, err := ca.NewVirtualSigstore()
	require.NoError(t, err)
	entity, err := authority.Sign("https://github.com/"+publisher+"/.github/workflows/release.yaml@refs/tags/v"+releaseVersion, "https://token.actions.githubusercontent.com", content)
	require.NoError(t, err)
	verification, err := entity.VerificationContent()
	require.NoError(t, err)
	signature, err := entity.SignatureContent()
	require.NoError(t, err)
	b := &bundleproto.Bundle{
		MediaType:            "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &bundleproto.VerificationMaterial{Content: &bundleproto.VerificationMaterial_Certificate{Certificate: &commonproto.X509Certificate{RawBytes: verification.Certificate().Raw}}},
		Content:              &bundleproto.Bundle_MessageSignature{MessageSignature: &commonproto.MessageSignature{MessageDigest: &commonproto.HashOutput{Algorithm: commonproto.HashAlgorithm_SHA2_256, Digest: signature.MessageSignatureContent().Digest()}, Signature: signature.Signature()}},
	}
	logs, err := entity.TlogEntries()
	require.NoError(t, err)
	for _, log := range logs {
		entry := log.TransparencyLogEntry()
		entry.KindVersion = &rekorproto.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
		proof, err := authority.GetInclusionProof(entry.CanonicalizedBody)
		require.NoError(t, err)
		rootHash, err := hex.DecodeString(*proof.RootHash)
		require.NoError(t, err)
		entry.LogIndex = *proof.LogIndex
		entry.InclusionProof = &rekorproto.InclusionProof{LogIndex: *proof.LogIndex, RootHash: rootHash, TreeSize: *proof.TreeSize, Checkpoint: &rekorproto.Checkpoint{Envelope: *proof.Checkpoint}}
		for _, hash := range proof.Hashes {
			decoded, err := hex.DecodeString(hash)
			require.NoError(t, err)
			entry.InclusionProof.Hashes = append(entry.InclusionProof.Hashes, decoded)
		}
		promise, err := authority.RekorSignPayload(tlog.RekorPayload{Body: base64.StdEncoding.EncodeToString(entry.CanonicalizedBody), IntegratedTime: entry.IntegratedTime, LogIndex: entry.LogIndex, LogID: hex.EncodeToString(entry.LogId.KeyId)})
		require.NoError(t, err)
		entry.InclusionPromise = &rekorproto.InclusionPromise{SignedEntryTimestamp: promise}
		b.VerificationMaterial.TlogEntries = append(b.VerificationMaterial.TlogEntries, entry)
	}
	timestamps, err := entity.Timestamps()
	require.NoError(t, err)
	b.VerificationMaterial.TimestampVerificationData = &bundleproto.TimestampVerificationData{}
	for _, stamp := range timestamps {
		b.VerificationMaterial.TimestampVerificationData.Rfc3161Timestamps = append(b.VerificationMaterial.TimestampVerificationData.Rfc3161Timestamps, &commonproto.RFC3161SignedTimestamp{SignedTimestamp: stamp})
	}
	encoded, err := protojson.Marshal(b)
	require.NoError(t, err)
	return encoded, authority
}

func TestVerifierRequiresAuthenticatedBundle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*Verifier, *bundleproto.Bundle, []byte) []byte
		version string
		pass    bool
		reason  string
	}{
		{name: "valid", version: "1.2.3", pass: true},
		{name: "producer SET without TSA", version: "1.2.3", pass: true, change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.VerificationMaterial.TimestampVerificationData = nil
			return c
		}},
		{name: "legacy bundle", version: "1.2.3", reason: "requires Sigstore bundle v0.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.MediaType = "application/vnd.dev.sigstore.bundle+json;version=0.1"
			for _, entry := range b.VerificationMaterial.TlogEntries {
				entry.InclusionProof = nil
			}
			cert := b.VerificationMaterial.GetCertificate()
			b.VerificationMaterial.Content = &bundleproto.VerificationMaterial_X509CertificateChain{X509CertificateChain: &commonproto.X509CertificateChain{Certificates: []*commonproto.X509Certificate{cert}}}
			return c
		}},
		{name: "wrong version", version: "1.2.4"},
		{name: "different publisher", version: "1.2.3", change: func(v *Verifier, _ *bundleproto.Bundle, c []byte) []byte {
			v.workflow = "https://github.com/attacker/router/.github/workflows/release.yaml"
			return c
		}},
		{name: "wrong issuer", version: "1.2.3", change: func(v *Verifier, _ *bundleproto.Bundle, c []byte) []byte {
			v.issuer = "https://attacker.invalid"
			return c
		}},
		{name: "missing signing time", version: "1.2.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.VerificationMaterial.TimestampVerificationData = nil
			for _, entry := range b.VerificationMaterial.TlogEntries {
				entry.InclusionPromise = nil
			}
			return c
		}},
		{name: "untrusted chain", version: "1.2.3", change: func(v *Verifier, _ *bundleproto.Bundle, c []byte) []byte { v.trustedMaterial = nil; return c }},
		{name: "missing transparency", version: "1.2.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.VerificationMaterial.TlogEntries = nil
			return c
		}},
		{name: "tampered inclusion proof", version: "1.2.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.VerificationMaterial.TlogEntries[0].InclusionProof.RootHash[0] ^= 1
			return c
		}},
		{name: "tampered signing time", version: "1.2.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.VerificationMaterial.TimestampVerificationData = nil
			b.VerificationMaterial.TlogEntries[0].InclusionPromise.SignedEntryTimestamp[0] ^= 1
			return c
		}},
		{name: "tampered signature", version: "1.2.3", change: func(_ *Verifier, b *bundleproto.Bundle, c []byte) []byte {
			b.GetMessageSignature().Signature[0] ^= 1
			return c
		}},
		{name: "tampered checksums", version: "1.2.3", change: func(_ *Verifier, _ *bundleproto.Bundle, c []byte) []byte { return append(c, 'x') }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("abc123  file.tar.gz\n")
			encoded, material := signedUpdateChecksums(t, content, "1.2.3")
			v := NewVerifier("stperic", "zzrouter")
			v.trustedMaterial = material
			var b bundleproto.Bundle
			require.NoError(t, protojson.Unmarshal(encoded, &b))
			if tc.change != nil {
				content = tc.change(v, &b, content)
			}
			encoded, err := protojson.Marshal(&b)
			require.NoError(t, err)
			dir := t.TempDir()
			checksums := filepath.Join(dir, "checksums.txt")
			bundle := filepath.Join(dir, "checksums.txt.sigstore.json")
			require.NoError(t, os.WriteFile(checksums, content, 0600))
			require.NoError(t, os.WriteFile(bundle, encoded, 0600))
			err = v.VerifySignature(checksums, bundle, tc.version)
			if tc.pass {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if tc.reason != "" {
					require.ErrorContains(t, err, tc.reason)
				}
			}
		})
	}
}

func TestVerifierNeverApprovesUnsignedRelease(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "file.tar.gz")
	content := []byte("archive")
	require.NoError(t, os.WriteFile(artifact, content, 0600))
	digest := sha256.Sum256(content)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(hex.EncodeToString(digest[:])+"  file.tar.gz\n"), 0600))
	for _, path := range []string{"", filepath.Join(dir, "missing"), filepath.Join(dir, "malformed")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "malformed"), []byte(`{"signature":"untrusted"}`), 0600))
			got, err := NewVerifier("stperic", "zzrouter").Verify(&DownloadResult{FilePath: artifact}, path, "1.2.3")
			require.NoError(t, err)
			require.False(t, got.Valid)
			require.False(t, got.SignatureValid)
			require.Contains(t, got.Error, "signature verification failed")
		})
	}
}

func TestVerifierUsesTheAuthenticatedChecksumBytes(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "file.tar.gz")
	require.NoError(t, os.WriteFile(artifact, []byte("archive"), 0600))
	checksums := []byte(strings.Repeat("0", 64) + "  file.tar.gz\n")
	encoded, material := signedUpdateChecksums(t, checksums, "1.2.3")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "checksums.txt"), checksums, 0600))
	bundle := filepath.Join(dir, "checksums.txt.sigstore.json")
	require.NoError(t, os.WriteFile(bundle, encoded, 0600))
	v := NewVerifier("stperic", "zzrouter")
	v.trustedMaterial = material
	result, err := v.Verify(&DownloadResult{FilePath: artifact}, bundle, "1.2.3")
	require.NoError(t, err)
	require.True(t, result.SignatureValid)
	require.False(t, result.ChecksumValid)
	require.False(t, result.Valid)
}

func TestEmbeddedSigstoreTrustIsValid(t *testing.T) {
	material, err := root.NewTrustedRootFromJSON([]byte(sigstoreTrustedRoot))
	require.NoError(t, err)
	require.NotEmpty(t, material.FulcioCertificateAuthorities())
	require.NotEmpty(t, material.RekorLogs())
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(sigstoreTrustedRoot), &metadata))
	require.Equal(t, "application/vnd.dev.sigstore.trustedroot+json;version=0.1", metadata["mediaType"])
}

func TestParseChecksumsRejectsDuplicateAssets(t *testing.T) {
	_, err := parseChecksums([]byte("abc  archive.tar.gz\ndef  archive.tar.gz\n"))
	require.ErrorContains(t, err, "duplicate")
}

func TestDefaultPublisherIdentity(t *testing.T) {
	checker := NewChecker("stable", "")
	for _, publisher := range []string{"stperic/zzrouter", "zzfactor/router"} {
		t.Run(publisher, func(t *testing.T) {
			content := []byte("abc123  file.tar.gz\n")
			encoded, material := signedUpdateChecksumsFrom(t, content, "1.2.3", publisher)
			v := NewVerifier(checker.owner, checker.repo)
			v.trustedMaterial = material
			dir := t.TempDir()
			checksums := filepath.Join(dir, "checksums.txt")
			bundlePath := filepath.Join(dir, "checksums.txt.sigstore.json")
			require.NoError(t, os.WriteFile(checksums, content, 0600))
			require.NoError(t, os.WriteFile(bundlePath, encoded, 0600))
			err := v.VerifySignature(checksums, bundlePath, "1.2.3")
			if publisher == "stperic/zzrouter" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "authenticate release checksums")
			}
		})
	}
}
