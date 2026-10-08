package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// CertPaths holds the file paths for generated certificates.
type CertPaths struct {
	CACert     string // ca.pem
	CAKey      string // ca-key.pem
	ServerCert string // server.pem
	ServerKey  string // server-key.pem
}

// GenerateSelfSignedCerts creates a CA and a server certificate signed by that CA.
// The server cert includes the given hostnames and IPs as SANs.
// Files are written to outDir with restrictive permissions (0644 for certs, 0600 for keys).
func GenerateSelfSignedCerts(outDir string, hostnames []string, ips []net.IP, validDays int) (*CertPaths, error) {
	if err := os.MkdirAll(outDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create cert directory: %w", err)
	}

	paths := &CertPaths{
		CACert:     filepath.Join(outDir, "ca.pem"),
		CAKey:      filepath.Join(outDir, "ca-key.pem"),
		ServerCert: filepath.Join(outDir, "server.pem"),
		ServerKey:  filepath.Join(outDir, "server-key.pem"),
	}

	// Generate CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA key: %w", err)
	}

	caSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA serial number: %w", err)
	}

	now := utils.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "zzrouter-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(time.Duration(validDays) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Generate server key
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate server key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	serverNow := utils.Now()
	serverTemplate := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "zzrouter"},
		NotBefore:    serverNow,
		NotAfter:     serverNow.Add(time.Duration(validDays) * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     hostnames,
		IPAddresses:  ips,
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create server certificate: %w", err)
	}

	// Write files
	if err := writePEM(paths.CACert, "CERTIFICATE", caCertDER, 0644); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(paths.CAKey, caKey, 0600); err != nil {
		return nil, err
	}
	if err := writePEM(paths.ServerCert, "CERTIFICATE", serverCertDER, 0644); err != nil {
		return nil, err
	}
	if err := writeKeyPEM(paths.ServerKey, serverKey, 0600); err != nil {
		return nil, err
	}

	return paths, nil
}

func writePEM(path, blockType string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

func writeKeyPEM(path string, key *ecdsa.PrivateKey, perm os.FileMode) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	return writePEM(path, "EC PRIVATE KEY", der, perm)
}
