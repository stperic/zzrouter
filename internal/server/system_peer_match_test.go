package server

import (
	"crypto/x509"
	"net"
	"testing"
)

// TestNodeURLMatchesPeerCert covers the SAN-match contract used by the
// /internal/models/refresh narrowing path: host from node_url must
// appear as a DNSName (for hostnames) or IPAddress SAN on the peer's
// leaf cert, otherwise the handler must fall back to broadcast.
func TestNodeURLMatchesPeerCert(t *testing.T) {
	cert := &x509.Certificate{
		DNSNames:    []string{"worker-1.local"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("::1")},
	}

	cases := []struct {
		name    string
		nodeURL string
		want    bool
	}{
		{"ip_san_match", "http://192.0.2.10:9090", true},
		{"ipv6_san_match", "http://[::1]:9090", true},
		{"dns_san_match", "https://worker-1.local:9091", true},
		{"ip_san_mismatch", "http://10.9.9.9:9090", false},
		{"dns_san_mismatch", "http://other-worker.local:9090", false},
		{"empty_url", "", false},
		{"malformed_url", "::not-a-url::", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeURLMatchesPeerCert(tc.nodeURL, cert)
			if got != tc.want {
				t.Errorf("nodeURLMatchesPeerCert(%q) = %v, want %v",
					tc.nodeURL, got, tc.want)
			}
		})
	}
}

// TestNodeURLMatchesPeerCert_NilCertRejects locks the fail-closed
// contract: no cert means no narrowing, regardless of the URL.
func TestNodeURLMatchesPeerCert_NilCertRejects(t *testing.T) {
	if nodeURLMatchesPeerCert("http://192.0.2.10:9090", nil) {
		t.Fatal("nil cert must not match any URL")
	}
}
