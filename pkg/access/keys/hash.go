package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters — lighter than password-grade because API keys are
// high-entropy (32 bytes of crypto/rand), so brute force is infeasible.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16

	// RawKeyLen is the length of generated raw API keys in bytes (before encoding).
	RawKeyLen = 32

	// KeyPrefix is prepended to generated keys for easy identification.
	KeyPrefix = "zzr_"
)

// GenerateRawKey creates a new cryptographically random API key.
// Returns a prefixed, base64url-encoded key (e.g., "zzr_abc123...").
func GenerateRawKey() (string, error) {
	b := make([]byte, RawKeyLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random key: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashKey hashes a raw API key using Argon2id.
func HashKey(rawKey string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(rawKey), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// Encode as: $argon2id$v=19$m=65536,t=1,p=2$<salt>$<hash>
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyKey checks a raw key against an Argon2id hash string.
func VerifyKey(rawKey, encodedHash string) bool {
	// Parse the encoded hash
	var version int
	var memory, time, threadsU32 uint32
	var saltB64, hashB64 string

	n, err := fmt.Sscanf(encodedHash, "$argon2id$v=%d$m=%d,t=%d,p=%d$%s",
		&version, &memory, &time, &threadsU32, &saltB64)
	if err != nil || n < 5 {
		return false
	}

	// saltB64 contains "salt$hash", split on the last $
	idx := strings.LastIndex(saltB64, "$")
	if idx < 0 {
		return false
	}
	hashB64 = saltB64[idx+1:]
	saltB64 = saltB64[:idx]

	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	if err != nil {
		return false
	}

	expectedHash, err := base64.RawStdEncoding.DecodeString(hashB64)
	if err != nil {
		return false
	}

	computed := argon2.IDKey([]byte(rawKey), salt, time, memory, uint8(threadsU32), uint32(len(expectedHash)))

	// Constant-time comparison
	if len(computed) != len(expectedHash) {
		return false
	}
	var diff byte
	for i := range computed {
		diff |= computed[i] ^ expectedHash[i]
	}
	return diff == 0
}

// SHA256Fingerprint returns the SHA256 hash of a raw key, used as a cache key.
func SHA256Fingerprint(rawKey string) [32]byte {
	return sha256.Sum256([]byte(rawKey))
}
