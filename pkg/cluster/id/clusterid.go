// Package clusterid provides identity and certificate authority primitives
// for the zzrouter cluster. It has no HTTP, gin, or configuration dependencies
// and is safe to import from pkg/clusternode without pulling in the rest of
// the server.
//
// Two main types:
//
//   - Identity — a node's long-term ECDSA keypair plus its current cert
//     (self-signed while unclaimed, CA-signed once claimed). The keypair
//     is stable across renewals so the SPKI fingerprint is the node's
//     canonical identity.
//
//   - CA — the cluster's certificate authority, owned by the coordinator.
//     Signs worker and coordinator identity certs with a fixed OU and TTL
//     derived from the Role.
package clusterid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/common/atomicfile"
)

// Role distinguishes the two kinds of identity certs issued by the CA. The
// role determines both the cert's Organizational Unit (OU) — which every
// mTLS middleware checks — and its TTL.
type Role int

const (
	// RoleWorker is a paired worker node. OU=zzrouter-worker, TTL=30d,
	// auto-renewed by the worker at 15d remaining.
	RoleWorker Role = iota + 1
	// RoleCoordinator is the cluster coordinator. OU=zzrouter-coordinator,
	// TTL=20y (the coordinator's identity is re-issued only on rotate-ca).
	RoleCoordinator
)

// OU returns the Organizational Unit string embedded in the cert subject.
// Every mTLS middleware checks this to distinguish coordinator from worker
// client certs.
func (r Role) OU() string {
	switch r {
	case RoleWorker:
		return "zzrouter-worker"
	case RoleCoordinator:
		return "zzrouter-coordinator"
	default:
		return ""
	}
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	return r == RoleWorker || r == RoleCoordinator
}

// Sentinel errors. Callers match with errors.Is.
var (
	ErrInvalidRole    = errors.New("clusterid: invalid role")
	ErrInvalidCSR     = errors.New("clusterid: invalid CSR")
	ErrWildcardSAN    = errors.New("clusterid: wildcard SAN not permitted")
	ErrKeyMismatch    = errors.New("clusterid: cert public key does not match identity key")
	ErrBadCertPEM     = errors.New("clusterid: malformed certificate PEM")
	ErrBadKeyPEM      = errors.New("clusterid: malformed private key PEM")
	ErrUnsafeSAN      = errors.New("clusterid: CSR requests unsafe IP SAN")
	ErrUnsupportedKey = errors.New("clusterid: CSR public key is not ECDSA P-256")
)

// Fingerprint returns the cert's SPKI SHA256 fingerprint in the canonical
// "sha256:<hex>" form. Stable across renewals because the keypair is reused.
func Fingerprint(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SPKIFingerprintHexLen is the length of a canonical SHA256 SPKI
// fingerprint in hex form (32 bytes → 64 hex chars).
const SPKIFingerprintHexLen = 64

// malformedFingerprintMsg prefixes both NormalizeFingerprint rejection
// paths so the error message is consistent across length-check and
// hex-decode failures.
const malformedFingerprintMsg = "clusterid: fingerprint must be 64 hex chars (optional sha256: prefix)"

// NormalizeFingerprint accepts a SPKI SHA256 fingerprint in either
// "sha256:<hex>" or bare "<hex>" form and returns the lowercase hex
// suffix. Input is trimmed and lowercased before validation so paste-
// from-display is operator-friendly. Non-hex characters (e.g. the
// "…" ellipsis from ShortForm output) are rejected — deny-list
// writers must persist only canonical 64-char hex.
//
// Single source of truth for "is this a valid fingerprint?" — used
// by the revoke-worker admin handler and the pairing-client CA pin
// normalizer. Both call sites need identical format semantics so
// reusing one function keeps them in sync.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) != SPKIFingerprintHexLen {
		return "", fmt.Errorf("%s: got %d chars", malformedFingerprintMsg, len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("%s: %w", malformedFingerprintMsg, err)
	}
	return s, nil
}

// ShortForm returns the first-8 + last-8 hex-char truncation of a
// "sha256:<hex>" fingerprint, joined by the single-character ellipsis
// "…" (U+2026). Used for visual comparison between two terminals
// during pairing-accept: clipboard malware can swap the full
// fingerprint but cannot make two different SPKIs produce the same
// 16-hex-char short form.
//
// Returns "" if fp is not in the expected "sha256:<hex>" shape with a
// hex-only suffix of at least 16 chars.
func ShortForm(fp string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(fp, prefix) {
		return ""
	}
	h := fp[len(prefix):]
	if len(h) < 16 {
		return ""
	}
	if _, err := hex.DecodeString(h); err != nil {
		return ""
	}
	return h[:8] + "…" + h[len(h)-8:]
}

// generateECDSAKey generates a fresh P-256 keypair. P-256 balances broad
// compatibility, small cert sizes, and fast handshakes; zero reason to use
// anything else for an internal cluster.
func generateECDSAKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// randomSerial returns a cryptographically random 128-bit positive serial,
// as required by RFC 5280 §4.1.2.2 for non-CA signing flexibility.
func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("random serial: %w", err)
	}
	return n, nil
}

// writeIdentityFile creates the private identity directory before durable replacement.
func writeIdentityFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	return atomicfile.WriteFile(path, data, 0o600)
}

// encodePEM wraps der bytes in a PEM block of the given type.
func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
