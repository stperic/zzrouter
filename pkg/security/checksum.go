package security

import (
	"io"

	"github.com/stperic/common/artifact"
)

// ErrChecksumMismatch distinguishes integrity failures from file I/O errors.
var ErrChecksumMismatch = artifact.ErrChecksumMismatch

// VerifySHA256 checks a file against its expected digest.
func VerifySHA256(path, expected string) error { return artifact.VerifySHA256(path, expected) }

// VerifySHA256Reader checks a stream against its expected digest.
func VerifySHA256Reader(r io.Reader, expected string) error {
	return artifact.VerifySHA256Reader(r, expected)
}

// ComputeSHA256 returns a file's SHA256 digest.
func ComputeSHA256(path string) (string, error) { return artifact.ComputeSHA256(path) }
