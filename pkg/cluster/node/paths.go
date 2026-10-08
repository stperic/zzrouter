package clusternode

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// errCoordinatorURLEmpty is returned by ReadCoordinatorURL when the
// coordinator_url file exists but contains only whitespace. File-local
// and unexported — callers only need errors.Is discrimination for the
// one test that distinguishes "missing" (os.ErrNotExist propagated)
// from "present but blank".
var errCoordinatorURLEmpty = errors.New("coordinator_url is empty")

// On-disk layout under the XDG config directory:
//
//	<config-dir>/
//	  identity/        node.key + node.pem (0600 key, 0700 dir)
//	  ca/              ca.key + ca.pem (coordinator only; 0700 dir, 0600 key)
//	  cluster/         ca.pem + coordinator_url (worker post-pairing, 0700 dir)
//	  pairing.txt      unclaimed-only pairing code (0600, written by
//	                   BeginPairing, removed on pairing complete or cancel)
//
// clusternode owns the permission enforcement — clusterid.Load* writes
// with strict perms and writePairingCodeFile writes 0600 atomic, so
// the adapter only has to supply the paths.
const (
	identitySubdir = "identity"
	caSubdir       = "ca"
	clusterSubdir  = "cluster"
	pairingFile    = "pairing.txt"

	caFile             = "ca.pem"
	nodeCertFile       = "node.pem"
	coordinatorURLFile = "coordinator_url"
)

// Paths bundles the XDG-derived locations both the Config adapter and
// the role derivation consume. Pure derived values — no I/O — so
// callers can share the struct freely.
type Paths struct {
	IdentityDir string
	CADir       string
	ClusterDir  string
	PairingPath string
}

// PathsFromConfigDir derives the canonical on-disk layout from the
// configured XDG directory. It does no I/O.
func PathsFromConfigDir(configDir string) Paths {
	return Paths{
		IdentityDir: filepath.Join(configDir, identitySubdir),
		CADir:       filepath.Join(configDir, caSubdir),
		ClusterDir:  filepath.Join(configDir, clusterSubdir),
		PairingPath: filepath.Join(configDir, pairingFile),
	}
}

// IsPaired reports whether the on-disk state represents a valid prior
// pairing that can resume as Worker. Three things must hold:
//
//  1. cluster/ca.pem is present.
//  2. cluster/coordinator_url is present and non-empty.
//  3. identity/node.pem chains to cluster/ca.pem and is not expired
//     (verify ignores KeyUsages so a Worker-OU cert with client+server
//     auth passes).
//
// Any missing piece — corrupted pairing state, stale artefacts from
// a prior install, migrated-coordinator mismatch — falls back to
// Unclaimed and lets the operator re-pair cleanly. Without the chain
// check a stale identity cert signed by a different CA (or expired)
// would boot as Worker and spend the first renewal cycle in a live-
// but-broken state before reverting.
func IsPaired(p Paths) bool {
	caPath := filepath.Join(p.ClusterDir, caFile)
	if _, err := os.Stat(caPath); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(p.ClusterDir, coordinatorURLFile)); err != nil {
		return false
	}
	caCert, err := loadCertFromPEM(caPath)
	if err != nil {
		return false
	}
	identityCert, err := loadCertFromPEM(filepath.Join(p.IdentityDir, nodeCertFile))
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := identityCert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return false
	}
	return true
}

// ReadCoordinatorURL reads clusterDir/coordinator_url. IsPaired
// already confirms presence; this is the separate read step so the
// Mode branch can return an error cleanly without the probe.
func ReadCoordinatorURL(clusterDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(clusterDir, coordinatorURLFile))
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(string(data))
	if url == "" {
		return "", errCoordinatorURLEmpty
	}
	return url, nil
}

// ParseAdvertiseIPs converts the YAML string slice into net.IP values,
// silently dropping anything net.ParseIP can't handle. A misconfigured
// entry is a single-node deployment error that surfaces loudly enough
// at pairing time (SAN mismatch on mTLS dispatch); no need to fail the
// whole boot over it. Empty input returns nil, which clusternode.Config
// interprets as "use the [127.0.0.1, ::1] default".
func ParseAdvertiseIPs(raw []string) []net.IP {
	if len(raw) == 0 {
		return nil
	}
	ips := make([]net.IP, 0, len(raw))
	for _, s := range raw {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil
	}
	return ips
}

// loadCertFromPEM reads a single CERTIFICATE PEM block from path.
// Intentionally narrow (one block, first match) so a file with
// trailing garbage or multiple blocks is still usable.
func loadCertFromPEM(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: expected CERTIFICATE PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}
