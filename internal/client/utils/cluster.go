package client

import (
	"context"
	"strings"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/utils"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// PairAcceptResult is the decoded success payload from POST
// /zzrouter/v1/cluster/pairing/accept. Fingerprint + ShortForm let
// operators visually cross-check the newly-paired worker against
// the code they read off its display.
type PairAcceptResult struct {
	NodeName    string `json:"node_name"`
	Fingerprint string `json:"fingerprint"`
	ShortForm   string `json:"short_form"`
}

// AcceptPairingCode completes a worker-initiated pairing handshake.
// The code may be supplied with dashes (e.g. ABCD-EFGH-IJKL-MNOP) —
// dashes and whitespace are stripped, the string is uppercased, and
// the server validates the final form.
func (c *Client) AcceptPairingCode(code, workerURL, workerFingerprint string) (*PairAcceptResult, error) {
	clean := strings.ToUpper(strings.TrimSpace(code))
	clean = strings.ReplaceAll(clean, "-", "")
	var out PairAcceptResult
	if err := c.doJSON("POST", apipath.ClusterPairingAccept, map[string]string{"code": clean, "worker_url": workerURL, "worker_fingerprint": workerFingerprint}, &out, "accept pairing code"); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveClusterEndpoint removes a worker's endpoint row from the
// cluster config. DELETE /zzrouter/v1/cluster/endpoints/:address
func (c *Client) RemoveClusterEndpoint(address string) error {
	addr := strings.TrimPrefix(address, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	return c.doJSON("DELETE", apipath.ClusterEndpoint(addr), nil, nil, "remove cluster endpoint")
}

// AcceptPairingTransfer sends an operator-reviewed document through the existing
// coordinator enrollment operation. Validation never approves a machine by itself.
func (c *Client) AcceptPairingTransfer(ctx context.Context, transfer clusternode.PairingTransfer) (*PairAcceptResult, error) {
	if err := transfer.Validate(utils.NowUTC()); err != nil {
		return nil, err
	}
	var out PairAcceptResult
	if err := c.doContextJSON(ctx, "POST", apipath.ClusterPairingAccept, map[string]any{"transfer": transfer}, &out, "accept pairing document"); err != nil {
		return nil, err
	}
	return &out, nil
}
