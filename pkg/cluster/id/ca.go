package clusterid

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CA file names on disk. Directory is caller-provided.
const (
	caKeyFile  = "ca.key"
	caCertFile = "ca.pem"
)

// CA TTLs. Coordinator self-signed CA cert gets a long life (20y) because
// the whole cluster's trust graph is rooted in it; a rotation is an
// explicit operator action. Worker certs get a short life (30d) with
// auto-renew.
const (
	caCertTTL          = 20 * 365 * 24 * time.Hour
	workerCertTTL      = 30 * 24 * time.Hour
	coordinatorCertTTL = caCertTTL
)

// TTL returns the signing TTL for a role.
func (r Role) TTL() time.Duration {
	switch r {
	case RoleWorker:
		return workerCertTTL
	case RoleCoordinator:
		return coordinatorCertTTL
	default:
		return 0
	}
}

// CA is the cluster certificate authority. Coordinator-owned; signs worker
// and coordinator identity certs with role-specific OU and TTL.
//
// The CA key is not encrypted on disk in v1. File perms (0600 + directory
// 0700) are the only defense against FS-level exfiltration; live-process
// compromise is out of scope until HSM support lands in v2.
type CA struct {
	key      *ecdsa.PrivateKey
	cert     *x509.Certificate
	certPEM  []byte
	dir      string
	keyPath  string
	certPath string
}

// LoadOrCreateCA loads the CA keypair and self-signed cert from dir,
// generating them on first coordinator boot. Directory is created 0700.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("ca mkdir: %w", err)
	}
	ca := &CA{
		dir:      dir,
		keyPath:  filepath.Join(dir, caKeyFile),
		certPath: filepath.Join(dir, caCertFile),
	}

	key, err := loadOrCreateKey(ca.keyPath)
	if err != nil {
		return nil, err
	}
	ca.key = key

	cert, certPEM, err := loadCertIfMatches(ca.certPath, key)
	if err != nil {
		return nil, err
	}
	if cert == nil {
		cert, certPEM, err = ca.selfSign()
		if err != nil {
			return nil, fmt.Errorf("ca self-sign: %w", err)
		}
		if err := writeAtomic(ca.certPath, certPEM, 0o600); err != nil {
			return nil, fmt.Errorf("write ca cert: %w", err)
		}
	}
	ca.cert = cert
	ca.certPEM = certPEM
	return ca, nil
}

// Certificate returns the parsed CA cert. Never nil after successful load.
// The CA's cert is immutable post-construction (rotate-ca builds a new CA
// object), so no mutex is needed.
func (ca *CA) Certificate() *x509.Certificate { return ca.cert }

// CertPEM returns a copy of the PEM-encoded CA cert bytes. Safe for the
// caller to retain or mutate.
func (ca *CA) CertPEM() []byte { return bytes.Clone(ca.certPEM) }

// Sign validates csrPEM and issues a cert for the given role. Enforces
// these unconditional invariants (fails closed):
//
//   - CSR PEM decodes cleanly and the CSR signature is valid.
//   - CSR's public key is ECDSA on the P-256 curve. Prevents a worker
//     from requesting a signed cert bound to a weaker curve or RSA key.
//   - No wildcard SANs (DNS names "*", "*.foo").
//   - No unspecified-address or non-unicast IP SANs (0.0.0.0, ::,
//     multicast, link-local-unspecified). A worker requesting 0.0.0.0
//     would get a cert valid for every IP; loopback and link-local
//     unicast are allowed because nodes legitimately advertise them.
//   - ExtKeyUsage is ServerAuth + ClientAuth so the same cert works on
//     both sides of the mTLS handshake. The combination means a
//     compromised worker cert is valid as both client and server for
//     its SANs — accepted tradeoff for the symmetry, and the deny list
//     closes the window on revocation.
//   - Role is known.
//
// SAN-subset validation (the cert's SANs must match what the worker
// declared in its pairing-request CSR) is a pairing-protocol check,
// not a CA invariant — it lives in pkg/cluster/node. This package
// enforces a security floor; the pairing accept handler tightens
// above that.
func (ca *CA) Sign(csrPEM []byte, role Role) ([]byte, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidRole, role)
	}
	csr, err := parseAndVerifyCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	if err := enforceCSRPublicKey(csr.PublicKey); err != nil {
		return nil, err
	}
	if err := rejectWildcardSANs(csr.DNSNames); err != nil {
		return nil, err
	}
	if err := rejectUnsafeIPs(csr.IPAddresses); err != nil {
		return nil, err
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	// X.509 NotBefore/NotAfter are wire values consumed immediately by
	// x509.CreateCertificate; pkg/utils import is avoided to keep
	// pkg/clusterid on stdlib-only (slog + lumberjack dep graph is
	// inappropriate for a crypto primitive).
	now := time.Now().UTC() //nolint:forbidigo // lint:allow time.Now — cert NotBefore/NotAfter wire values (no monotonic)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         csr.Subject.CommonName,
			OrganizationalUnit: []string{role.OU()},
		},
		NotBefore:   now,
		NotAfter:    now.Add(role.TTL()),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    csr.DNSNames,
		IPAddresses: csr.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign cert: %w", err)
	}
	return encodePEM("CERTIFICATE", der), nil
}

func (ca *CA) selfSign() (*x509.Certificate, []byte, error) {
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	// X.509 NotBefore/NotAfter are wire values consumed immediately by
	// x509.CreateCertificate; pkg/utils import is avoided to keep
	// pkg/clusterid on stdlib-only (slog + lumberjack dep graph is
	// inappropriate for a crypto primitive).
	now := time.Now().UTC() //nolint:forbidigo // lint:allow time.Now — cert NotBefore/NotAfter wire values (no monotonic)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "zzrouter-cluster-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(caCertTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, ca.key.Public(), ca.key)
	if err != nil {
		return nil, nil, fmt.Errorf("self-sign ca: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}
	return cert, encodePEM("CERTIFICATE", der), nil
}

func parseAndVerifyCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	block, rest := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, ErrInvalidCSR
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("%w: trailing bytes after CERTIFICATE REQUEST", ErrInvalidCSR)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: bad signature: %w", ErrInvalidCSR, err)
	}
	return csr, nil
}

// enforceCSRPublicKey rejects CSRs whose public key isn't ECDSA-P256.
// Prevents a downgrade to RSA or a weaker EC curve — this package generates
// P-256 keys and expects to sign for identities built on the same.
func enforceCSRPublicKey(pub any) error {
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: key type %T", ErrUnsupportedKey, pub)
	}
	if ec.Curve != elliptic.P256() {
		return fmt.Errorf("%w: curve %s", ErrUnsupportedKey, ec.Curve.Params().Name)
	}
	return nil
}

func rejectWildcardSANs(dnsNames []string) error {
	for _, name := range dnsNames {
		if name == "*" || strings.HasPrefix(name, "*.") {
			return fmt.Errorf("%w: %q", ErrWildcardSAN, name)
		}
	}
	return nil
}

// rejectUnsafeIPs blocks IP SANs that would make the cert dangerously
// broad. Loopback and link-local unicast are allowed (nodes legitimately
// bind to 127.0.0.1 and fe80::). Unspecified (0.0.0.0, ::), multicast, and
// broadcast are rejected — no legitimate cert ever binds to them.
func rejectUnsafeIPs(ips []net.IP) error {
	for _, ip := range ips {
		switch {
		case ip == nil:
			return fmt.Errorf("%w: nil address", ErrUnsafeSAN)
		case ip.IsUnspecified():
			return fmt.Errorf("%w: %s (unspecified)", ErrUnsafeSAN, ip)
		case ip.IsMulticast():
			return fmt.Errorf("%w: %s (multicast)", ErrUnsafeSAN, ip)
		case ip.Equal(net.IPv4bcast):
			return fmt.Errorf("%w: %s (broadcast)", ErrUnsafeSAN, ip)
		}
	}
	return nil
}
