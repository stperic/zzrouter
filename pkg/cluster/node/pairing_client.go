package clusternode

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

const pairingHTTPTimeout = 60 * time.Second

var (
	ErrPairingBadCA                  = errors.New("clusternode: returned CA does not match the configured fingerprint")
	ErrPairingCAFingerprintMalformed = errors.New("clusternode: coordinator CA fingerprint is malformed")
	ErrPairingStrictModeRequired     = errors.New("clusternode: coordinator requires a verified CA fingerprint")
)

func newCAPinnedClient(caFingerprint string) (*http.Client, error) {
	pin, err := normalizeCAFingerprint(caFingerprint)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // verification moved to VerifyPeerCertificate (CA fingerprint pinning); default chain trust isn't used
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyAgainstPinnedCA(rawCerts, pin)
		},
		SessionTicketsDisabled: true,
	}
	return &http.Client{
		Timeout: pairingHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

func normalizeCAFingerprint(s string) (string, error) {
	hexStr, err := clusterid.NormalizeFingerprint(s)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrPairingCAFingerprintMalformed, err)
	}
	return hexStr, nil
}

func verifyCAPEMSignsLeaf(caPEM []byte, leaf *x509.Certificate) error {
	if leaf == nil {
		return errors.New("tofu verify: no TLS leaf captured from handshake")
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return errors.New("tofu verify: ca_cert_pem is not valid PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("tofu verify: parse ca_cert_pem: %w", err)
	}
	if !caCert.BasicConstraintsValid || !caCert.IsCA {
		return errors.New("tofu verify: ca_cert_pem is not a CA (BasicConstraints / IsCA)")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("tofu verify: ca_cert_pem KeyUsage lacks CertSign: cannot serve as cluster trust root")
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		return fmt.Errorf("tofu verify: returned CA does not chain the TLS handshake leaf: %w", err)
	}
	return nil
}

func verifyAgainstPinnedCA(rawCerts [][]byte, pinHex string) error {
	if len(rawCerts) == 0 {
		return errors.New("peer presented no certificates")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for i, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse cert %d: %w", i, err)
		}
		certs = append(certs, c)
	}

	// Locate a root whose SPKI matches the pin. Usually last in the
	// chain; walk all entries defensively (some servers send the root
	// first or mid-chain).
	var root *x509.Certificate
	for _, c := range certs {
		if spkiMatches(c, pinHex) {
			root = c
			break
		}
	}
	if root == nil {
		return fmt.Errorf("%w: no cert in presented chain matches pin %s…", ErrPairingBadCA, pinHex[:8])
	}

	leaf := certs[0]
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for i := 1; i < len(certs); i++ {
		if certs[i] != root {
			intermediates.AddCert(certs[i])
		}
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("chain verify: %w", err)
	}
	return nil
}

func spkiMatches(cert *x509.Certificate, pinHex string) bool {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]) == pinHex
}

func verifyCAPEMMatchesPin(caPEM []byte, pinHex string) error {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("returned ca_cert_pem is not a CERTIFICATE PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse returned CA: %w", err)
	}
	if !spkiMatches(cert, pinHex) {
		return ErrPairingBadCA
	}
	return nil
}
