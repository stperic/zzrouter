package clusterid

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Identity file names on disk. Directory is caller-provided.
const (
	identityKeyFile  = "node.key"
	identityCertFile = "node.pem"
)

// Self-signed cert TTL used while unclaimed. Large enough that an unclaimed
// node sitting for days/weeks pre-pair doesn't need a re-sign; short enough
// that a forgotten unclaimed node cycles its self-signed cert rather than
// holding stale metadata forever.
const selfSignedTTL = 90 * 24 * time.Hour

// Identity holds a node's ECDSA-P256 keypair plus its current cert. The
// keypair is persisted once and reused across renewals so the SPKI
// fingerprint is stable. The cert is self-signed before pairing and CA-signed
// after.
//
// All accessors and InstallSignedCert are safe for concurrent use. A live
// TLS server's GetCertificate callback runs on the handshake goroutine
// concurrently with the renewal ticker's InstallSignedCert; the RWMutex
// serializes them.
type Identity struct {
	mu       sync.RWMutex
	key      *ecdsa.PrivateKey
	cert     *x509.Certificate
	certPEM  []byte
	dir      string
	keyPath  string
	certPath string
}

// LoadOrCreateIdentity loads the keypair and cert from dir, generating them
// on first use. The directory is created with 0700 perms if missing. Keys
// and certs are written 0600.
//
// If the cert is missing or its public key doesn't match the loaded private
// key (e.g. a half-written rename), a new self-signed cert is generated
// against the current key. The fingerprint therefore depends only on the
// key; SPKI is stable across cert-only resets.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("identity mkdir: %w", err)
	}
	id := &Identity{
		dir:      dir,
		keyPath:  filepath.Join(dir, identityKeyFile),
		certPath: filepath.Join(dir, identityCertFile),
	}

	key, err := loadOrCreateKey(id.keyPath)
	if err != nil {
		return nil, err
	}
	id.key = key

	cert, certPEM, err := loadCertIfMatches(id.certPath, key)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		cert, certPEM, err = id.selfSign()
		if err != nil {
			return nil, fmt.Errorf("identity self-sign: %w", err)
		}
		if err := writeAtomic(id.certPath, certPEM, 0o600); err != nil {
			return nil, fmt.Errorf("write cert: %w", err)
		}
	}
	id.cert = cert
	id.certPEM = certPEM
	return id, nil
}

// Fingerprint returns the SPKI SHA256 in "sha256:<hex>" form. Stable across
// renewals because the keypair is reused.
func (id *Identity) Fingerprint() string {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return Fingerprint(id.cert)
}

// ShortForm returns the first-8 + last-8 hex truncation of the fingerprint
// for visual comparison.
func (id *Identity) ShortForm() string { return ShortForm(id.Fingerprint()) }

// Certificate returns the current parsed cert. Never nil after a successful
// LoadOrCreateIdentity. The returned pointer may be freely read; do not
// mutate it.
func (id *Identity) Certificate() *x509.Certificate {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return id.cert
}

// CertPEM returns a copy of the PEM-encoded cert bytes. Safe for the caller
// to retain or mutate.
func (id *Identity) CertPEM() []byte {
	id.mu.RLock()
	defer id.mu.RUnlock()
	return bytes.Clone(id.certPEM)
}

// Signer exposes the long-term keypair as a crypto.Signer. This is the
// narrowest interface that covers both tls.Config assembly (crypto.Signer
// satisfies crypto.PrivateKey) and CSR generation
// (x509.CreateCertificateRequest takes crypto.Signer). Callers should prefer
// this to holding *ecdsa.PrivateKey directly — it prevents accidental
// serialization or pretty-printing of secret material.
func (id *Identity) Signer() crypto.Signer { return id.key }

// CSR generates a PEM-encoded certificate signing request for the given
// common name and SANs. Used during claim (unclaimed→worker) and at each
// renewal. The CSR carries the node's long-term public key; the CA signs it
// with a role-specific OU and TTL.
func (id *Identity) CSR(commonName string, dnsNames []string, ipAddresses []net.IP) ([]byte, error) {
	id.mu.RLock()
	defer id.mu.RUnlock()
	tmpl := &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: commonName},
		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, id.key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return encodePEM("CERTIFICATE REQUEST", der), nil
}

// InstallSignedCert validates and installs a CA-signed cert returned from
// pairing accept or renew. Invariants:
//
//   - The PEM must decode to exactly one CERTIFICATE block.
//   - The cert's public key must match our long-term key. This rejects a
//     coordinator (or MITM) trying to hand us a cert for a different key.
//
// The file is replaced atomically; the next handshake picks it up via
// GetCertificate without a listener rebind.
//
// SAN-subset, OU, and chain-to-CA checks are the caller's job — they
// require context this package doesn't hold (the advertised SAN set, the
// trusted CA pool).
func (id *Identity) InstallSignedCert(certPEM []byte) error {
	cert, err := parseSingleCert(certPEM)
	if err != nil {
		return err
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if !samePublicKey(cert.PublicKey, id.key.Public()) {
		return ErrKeyMismatch
	}
	if err := writeAtomic(id.certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("write signed cert: %w", err)
	}
	id.cert = cert
	id.certPEM = bytes.Clone(certPEM)
	return nil
}

// selfSign issues a minimal self-signed cert used by an Unclaimed
// node's identity keypair until pairing installs a CA-signed
// replacement. SANs are localhost only; the cert is never presented
// on a live listener (Unclaimed is dormant on the cluster network),
// so SAN matching is not load-bearing.
func (id *Identity) selfSign() (*x509.Certificate, []byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	// X.509 NotBefore/NotAfter are wire values consumed immediately by
	// x509.CreateCertificate; pkg/utils import is avoided to keep
	// pkg/clusterid on stdlib-only.
	now := time.Now().UTC() //nolint:forbidigo // lint:allow time.Now — cert NotBefore/NotAfter wire values (no monotonic)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "zzrouter-unclaimed"},
		NotBefore:    now,
		NotAfter:     now.Add(selfSignedTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, id.key.Public(), id.key)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse self-signed: %w", err)
	}
	return cert, encodePEM("CERTIFICATE", der), nil
}

// loadOrCreateKey loads an ECDSA key from path, generating and persisting a
// new one if the file is missing. Any parse error returns an error — we do
// not silently overwrite a file we can't read, since that could destroy a
// valid key mis-tagged by a future format change.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, err := parseECPrivateKeyPEM(data)
		if err != nil {
			return nil, fmt.Errorf("parse existing key %s: %w", path, err)
		}
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		key, err := generateECDSAKey()
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("marshal key: %w", err)
		}
		pemBytes := encodePEM("EC PRIVATE KEY", der)
		if err := writeAtomic(path, pemBytes, 0o600); err != nil {
			return nil, fmt.Errorf("write key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
}

// loadCertIfMatches reads a cert from path and returns it only if its
// public key matches key. Returns (nil, nil, nil) if the file is missing
// or the cert doesn't match (we'll regenerate a self-signed cert in that
// case). Returns an error only on malformed file contents.
func loadCertIfMatches(path string, key *ecdsa.PrivateKey) (*x509.Certificate, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read cert %s: %w", path, err)
	}
	cert, err := parseSingleCert(data)
	if err != nil {
		return nil, nil, err
	}
	if !samePublicKey(cert.PublicKey, key.Public()) {
		return nil, nil, nil
	}
	return cert, data, nil
}

func parseSingleCert(pemBytes []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, ErrBadCertPEM
	}
	if len(rest) > 0 {
		// Extra data after the cert is suspicious — we only issue and
		// install single-cert PEM files. Reject rather than silently
		// ignore.
		return nil, fmt.Errorf("%w: trailing bytes after CERTIFICATE", ErrBadCertPEM)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCertPEM, err)
	}
	return cert, nil
}

func parseECPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, ErrBadKeyPEM
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadKeyPEM, err)
	}
	return key, nil
}

func samePublicKey(a, b any) bool {
	ap, ok := a.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	bp, ok := b.(*ecdsa.PublicKey)
	if !ok {
		return false
	}
	return ap.Equal(bp)
}
