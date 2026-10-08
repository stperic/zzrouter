package security

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSClientConfig builds a *tls.Config that trusts the given CA certificate file.
// If caCertPath is empty, returns nil (use system trust store).
func TLSClientConfig(caCertPath string) (*tls.Config, error) {
	if caCertPath == "" {
		return nil, nil
	}

	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate %s: %w", caCertPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("CA certificate %s contains no valid PEM certificates", caCertPath)
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}, nil
}
