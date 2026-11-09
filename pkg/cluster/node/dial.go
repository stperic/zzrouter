package clusternode

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
)

// DialClient returns the coordinator's mTLS dispatch client. Workers never dial peers.
// Callers own request deadlines so long inference streams have no global ceiling.
func (n *Node) DialClient(expectedPeerOU string) (*http.Client, error) {
	if n.Mode() != Coordinator {
		return nil, fmt.Errorf("%w: only the coordinator initiates cluster connections", ErrInvalidConfig)
	}
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
			DialContext:         (&net.Dialer{Timeout: constants.ClusterDialTimeout}).DialContext,
			TLSHandshakeTimeout: constants.ClusterTLSHandshakeTimeout,
			TLSClientConfig:     tlsCfg,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

// outboundTrustPool resolves the coordinator's trust root.
func (n *Node) outboundTrustPool() (*x509.CertPool, error) {
	if n.Mode() != Coordinator || n.ca == nil {
		return nil, fmt.Errorf("%w: coordinator CA unavailable", ErrInvalidConfig)
	}
	pool := x509.NewCertPool()
	pool.AddCert(n.ca.Certificate())
	return pool, nil
}
