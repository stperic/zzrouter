// Coordinator → worker decommission helper. Complement to the
// worker-initiated pairing flow that transitions Unclaimed→Worker;
// DecommissionWorker sends /cluster/leave to flip Worker→Unclaimed.
//
// The deny-list path (Revoke) is separate: decommission is a friendly
// "step out of the cluster" (worker reverts to dormant Unclaimed and
// keeps its identity key for re-pairing later); revoke is an
// adversarial deny that will cause renewal attempts to 403.

package clusternode

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
)

// DecommissionWorker POSTs /cluster/leave to the worker at addr over
// mTLS. The worker will ACK and self-heal back to Unclaimed — the
// identity keypair is preserved so the fingerprint survives and the
// operator can re-pair the same node later.
//
// addr is the worker's cluster-port base URL ("host:port"); the leading
// scheme is always https (cluster port is mTLS-only).
//
// Returns ErrNotCoordinator on non-coordinator nodes. Transport errors
// (worker offline, TLS handshake rejection) are returned wrapped.
func (n *Node) DecommissionWorker(ctx context.Context, addr string) error {
	if n.Mode() != Coordinator {
		return ErrNotCoordinator
	}
	client, err := n.DialClient(clusterid.RoleWorker.OU())
	if err != nil {
		return fmt.Errorf("build dispatch client: %w", err)
	}
	url := fmt.Sprintf("https://%s/zzrouter/v1/cluster/leave", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return fmt.Errorf("build leave request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch leave to %s: %w", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("worker rejected leave: status %d", resp.StatusCode)
	}
	return nil
}
