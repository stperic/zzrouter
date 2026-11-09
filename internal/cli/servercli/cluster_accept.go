package servercli

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// newClusterAcceptCmd wires `zzrouter cluster accept <code>`. Admin-
// keyed POST to the coord's /zzrouter/v1/cluster/pairing/accept. On
// success prints the node's short-form fingerprint so the operator can
// visually cross-check against the pairing-code display on the worker.
func newClusterAcceptCmd() *cobra.Command {
	var workerURL, workerFingerprint string
	cmd := &cobra.Command{
		Use:   "accept <code>",
		Short: "Accept a worker's pairing request",
		Long: `Accept a pending worker pairing request.

The code is printed by the worker when it enters pairing mode
(see 'zzrouter cluster pair' on the worker side). Paste it here to
complete the handshake. Supply the node URL and fingerprint printed on the
worker with --worker-url and --worker-fingerprint. The coordinator connects
to the worker, signs its CSR and delivers its certificate.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterAccept(args[0], workerURL, workerFingerprint)
		},
	}
	cmd.Flags().StringVar(&workerURL, "worker-url", "", "Armed worker HTTPS cluster endpoint")
	cmd.Flags().StringVar(&workerFingerprint, "worker-fingerprint", "", "Worker SPKI fingerprint verified from its local display")
	_ = cmd.MarkFlagRequired("worker-url")
	_ = cmd.MarkFlagRequired("worker-fingerprint")
	return cmd
}

// newClusterCAFingerprintCmd wires `zzrouter cluster ca-fingerprint`.
// Reads the coord's CA cert from the local filesystem and prints the
// SPKI SHA256 in canonical form plus the short display string. Does
// NOT make an HTTP call — this is the operator's way to capture the
// pin for worker config BEFORE any worker has been paired.
func newClusterCAFingerprintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ca-fingerprint",
		Short: "Print this coordinator's CA SPKI fingerprint",
		Long: `Print the coordinator's CA SPKI SHA256 fingerprint.

This value is the bootstrap pin workers use to verify the coordinator
during pairing. It is a hash of a public key, non-secret, durable,
and safe to bake into worker images, Ansible vars, or QR codes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterCAFingerprint()
		},
	}
}

func runClusterAccept(code, workerURL, workerFingerprint string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	// Allow operators to paste with dashes for readability; strip
	// them before sending. The server validates format anyway.
	code = strings.ReplaceAll(code, "-", "")

	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	localPort := cfg.Node.Port
	if localPort == 0 {
		localPort = constants.DefaultZZROUTERPort
	}
	adminKey, _ := cfg.Auth.GetAdminAPIKey()

	body, err := json.Marshal(map[string]string{"code": code, "worker_url": workerURL, "worker_fingerprint": workerFingerprint})
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/cluster/pairing/accept", localPort)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}

	client := &http.Client{Timeout: constants.HTTPLongTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local server unreachable (is it running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return clusterAcceptDecodeError(resp.StatusCode, respBody)
	}

	var out struct {
		NodeName    string `json:"node_name"`
		Fingerprint string `json:"fingerprint"`
		ShortForm   string `json:"short_form"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	fmt.Printf("Paired %s (%s)\n", out.NodeName, out.ShortForm)
	confirmWorkerReachable(localPort, adminKey, out.NodeName, clusterPortOf(cfg))
	return nil
}

// workerReachableTimeout bounds the wait for a freshly paired worker to
// report healthy. Generous, because the first health sweep after
// pairing has to complete before the registry can say anything.
const workerReachableTimeout = 20 * time.Second

// workerReachablePoll is how often the registry is re-read while waiting.
const workerReachablePoll = 2 * time.Second

// clusterPortOf returns the cluster listener port this coordinator uses,
// which is also the port it will dial on every worker.
func clusterPortOf(cfg *pkgConfig.NodeConfig) int {
	if cfg.Cluster.BindPort != 0 {
		return cfg.Cluster.BindPort
	}
	return constants.DefaultClusterPort
}

// confirmWorkerReachable checks membership and full health after pinned enrollment.
func confirmWorkerReachable(localPort int, adminKey, nodeName string, clusterPort int) {
	fmt.Printf("Checking that %s answers on the cluster port...\n", nodeName)

	deadline := utils.Now().Add(workerReachableTimeout)
	var last string
	for {
		health, err := fetchNodeHealth(localPort, adminKey, nodeName)
		if err == nil {
			last = health
			if health == healthStatusHealthy {
				fmt.Printf("%s is reachable and healthy.\n", nodeName)
				return
			}
		}
		if utils.Now().After(deadline) {
			break
		}
		time.Sleep(workerReachablePoll)
	}

	fmt.Println()
	fmt.Printf("WARNING: %s paired, but this coordinator cannot reach it", nodeName)
	if last != "" {
		fmt.Printf(" (health: %s)", last)
	}
	fmt.Println(".")
	fmt.Printf("Every request to it goes to %s:%d, not its admin port. Check that:\n", nodeName, clusterPort)
	fmt.Printf("  - the worker allows inbound TCP %d (its admin port is not enough)\n", clusterPort)
	fmt.Printf("  - the worker's cluster.bind_port is %d, matching this coordinator\n", clusterPort)
	fmt.Println("  - the worker is reachable at the address it advertised")
}

// healthStatusHealthy is the registry's value for a reachable node.
const healthStatusHealthy = "healthy"

// fetchNodeHealth reads one node's health from the local coordinator's
// registry.
func fetchNodeHealth(localPort int, adminKey, nodeName string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/nodes", localPort)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}
	resp, err := (&http.Client{Timeout: constants.ClusterQueryTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return decodeNodeHealth(resp.Body, nodeName)
}

// decodeNodeHealth pulls one named node's health out of a /nodes body.
func decodeNodeHealth(r io.Reader, nodeName string) (string, error) {
	var body struct {
		Data []struct {
			Name         string `json:"name"`
			HealthStatus string `json:"health_status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return "", err
	}
	for _, n := range body.Data {
		if strings.EqualFold(n.Name, nodeName) {
			return n.HealthStatus, nil
		}
	}
	return "", fmt.Errorf("node %q is not in the registry yet", nodeName)
}

func runClusterCAFingerprint() error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	if _, err := cm.LoadNodeConfig(); err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	caPath := filepath.Join(pkgConfig.Paths().GetConfigDir(), "ca", "ca.pem")
	data, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read CA cert at %s: %w (is this node a coordinator?)", caPath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%s: expected CERTIFICATE PEM", caPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse CA cert: %w", err)
	}
	fp := clusterid.Fingerprint(cert)
	fmt.Printf("fingerprint: %s\n", fp)
	fmt.Printf("short_form:  %s\n", clusterid.ShortForm(fp))
	return nil
}

// clusterAcceptDecodeError renders a server error body as a CLI error.
// Falls back to "HTTP <status>" if the body isn't JSON.
func clusterAcceptDecodeError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error != "" {
		return fmt.Errorf("server returned %d: %s", status, e.Error)
	}
	return fmt.Errorf("server returned %d", status)
}
