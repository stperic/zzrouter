package clusternode

import (
	"errors"
	"fmt"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

const pairingRequestMaxBodyBytes = 128 * 1024

type pairingRequestBody struct {
	NodeName           string   `json:"node_name"`
	Fingerprint        string   `json:"fingerprint"`
	CSRPEM             string   `json:"csr_pem"`
	SANs               []string `json:"sans"`
	Code               string   `json:"code"`
	ClusterProtocol    int      `json:"cluster_protocol"`
	MinClusterProtocol int      `json:"min_cluster_protocol"`
	BuildVersion       string   `json:"build_version,omitempty"`
	// CoordinatorCAFingerprint is the CA SPKI hash the worker pinned
	// on the bootstrap handshake. Empty when the worker ran in TOFU
	// mode. The coordinator compares this to its own fingerprint when
	// cluster.accept_insecure_pairing is false; empty or mismatched
	// values are rejected with 403. Honest-signal only (a malicious
	// worker can always lie) — the check gates operator intent, not
	// security per se.
	CoordinatorCAFingerprint string `json:"coordinator_ca_fingerprint,omitempty"`
}

type pairingApprovedResponse struct {
	Status                  string `json:"status"`
	SignedCertPEM           string `json:"signed_cert_pem"`
	CACertPEM               string `json:"ca_cert_pem"`
	CoordinatorURL          string `json:"coordinator_url"`
	ClusterProtocol         int    `json:"cluster_protocol"`
	MinClusterProtocol      int    `json:"min_cluster_protocol"`
	CoordinatorBuildVersion string `json:"coordinator_build_version,omitempty"`
}

// PairingAcceptance confirms an enrolled worker identity.
type PairingAcceptance struct {
	NodeName    string
	Fingerprint string
	ShortForm   string
	// RemoteAddr is the enrolled endpoint host used for membership admission.
	RemoteAddr string
}

func verifyWorkerPinnedOurCA(workerEcho, coordFingerprint string) error {
	if coordFingerprint == "" {
		// Should not happen — handler only runs on coordinators, so
		// CAFingerprint() returns non-empty. Guard defensively: if
		// the CA isn't loaded we cannot gate, so fail closed.
		return errors.New("coordinator CA not loaded; pairing unavailable")
	}
	if workerEcho == "" {
		return errors.New("this coordinator rejects TOFU pair requests; re-run with --secure + --ca-fingerprint")
	}
	// Defensive length cap. sha256 fingerprints are <100 chars even
	// with the "sha256:" prefix and any separators; anything longer
	// is either malformed or adversarial and shouldn't burn a
	// normalize-and-hex-decode pass.
	if len(workerEcho) > 256 {
		return errors.New("coordinator_ca_fingerprint too long")
	}
	workerNorm, err := clusterid.NormalizeFingerprint(workerEcho)
	if err != nil {
		return fmt.Errorf("invalid coordinator_ca_fingerprint: %w", err)
	}
	coordNorm, err := clusterid.NormalizeFingerprint(coordFingerprint)
	if err != nil {
		return fmt.Errorf("invalid coordinator fingerprint on server: %w", err)
	}
	if workerNorm != coordNorm {
		return errors.New("coordinator_ca_fingerprint does not match this coordinator's CA")
	}
	return nil
}

// CAFingerprint reports the coordinator trust root fingerprint.
func (n *Node) CAFingerprint() string {
	if n.ca == nil {
		return ""
	}
	return clusterid.Fingerprint(n.ca.Certificate())
}
