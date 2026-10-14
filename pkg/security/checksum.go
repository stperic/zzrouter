package security

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrChecksumMismatch distinguishes integrity failures from file I/O errors.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// VerifySHA256 checks that a file's SHA-256 hash matches the expected value.
func VerifySHA256(filePath, expectedHash string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return VerifySHA256Reader(f, expectedHash)
}

// VerifySHA256Reader checks a stream against the expected SHA-256 hash.
func VerifySHA256Reader(r io.Reader, expectedHash string) error {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return fmt.Errorf("failed to hash file: %w", err)
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	expected := strings.ToLower(strings.TrimSpace(expectedHash))

	if actual != expected {
		return fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expected, actual)
	}
	return nil
}

// ComputeSHA256 returns the hex-encoded SHA-256 hash of a file.
func ComputeSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
