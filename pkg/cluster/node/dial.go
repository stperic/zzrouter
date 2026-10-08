package clusternode

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DialClient returns an *http.Client for outbound mTLS dispatches from
// this node. It presents the node's identity as the client certificate
// and verifies peer certs chain to the node's trust pool. If
// expectedPeerOU is non-empty, VerifyConnection additionally asserts
// the peer leaf's Subject.OrganizationalUnit contains expectedPeerOU —
// the outbound mirror of mTLSOUCheck on the inbound side.
//
// Trust pool resolution:
//
//   - Coordinator: the in-memory CA loaded at New (used for dialing
//     workers; workers present CA-signed certs with OU=worker).
//   - Worker: the ca.pem persisted in ClusterDir at pairing time (used
//     for dialing the coordinator; the coordinator presents a CA-signed
//     cert with OU=coordinator).
//   - Disabled / Unclaimed: ErrInvalidConfig. Outbound mTLS is only
//     meaningful once a trust root exists.
//
// The returned client has no Timeout — callers bound requests via
// context deadlines so short health probes and long inference
// dispatches can share one client without a global ceiling.
//
// VerifyConnection (not VerifyPeerCertificate) is used for the OU
// check: VerifyPeerCertificate fires before the stdlib's chain
// validation, but VerifyConnection runs after, so by then
// cs.VerifiedChains[0][0] is a known-good leaf.
func (n *Node) DialClient(expectedPeerOU string) (*http.Client, error) {
	if n.identity == nil {
		return nil, fmt.Errorf("%w: node has no identity", ErrInvalidConfig)
	}
	pool, err := n.outboundTrustPool()
	if err != nil {
		return nil, err
	}
	leaf := n.identity.Certificate()
	cert := tls.Certificate{
		Certificate: [][]byte{leaf.Raw},
		PrivateKey:  n.identity.Signer(),
		Leaf:        leaf,
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
	}
	if expectedPeerOU != "" {
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
				return errors.New("no verified peer chain")
			}
			peer := cs.VerifiedChains[0][0]
			if !hasOU(peer, expectedPeerOU) {
				return fmt.Errorf("peer OU mismatch: want %q", expectedPeerOU)
			}
			return nil
		}
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

// outboundTrustPool resolves the CA pool this node uses to verify peer
// certificates on outbound dials. Coordinator uses the in-memory CA;
// Worker reads the ca.pem stored at pairing time. Other modes have no
// valid trust root for outbound mTLS.
func (n *Node) outboundTrustPool() (*x509.CertPool, error) {
	switch n.Mode() {
	case Coordinator:
		if n.ca == nil {
			return nil, fmt.Errorf("%w: coordinator has no CA", ErrInvalidConfig)
		}
		pool := x509.NewCertPool()
		pool.AddCert(n.ca.Certificate())
		return pool, nil
	case Worker:
		caPEM, err := os.ReadFile(filepath.Join(n.cfg.ClusterDir, "ca.pem"))
		if err != nil {
			return nil, fmt.Errorf("read stored CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("no valid CA cert in stored PEM")
		}
		return pool, nil
	default:
		return nil, fmt.Errorf("%w: mode %s does not support outbound mTLS", ErrInvalidConfig, n.Mode())
	}
}
