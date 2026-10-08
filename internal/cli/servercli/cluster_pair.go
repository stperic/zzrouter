package servercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/servercli/tui/pair"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/ui"
)

// mdnsDiscoveryTimeout caps the mDNS browse when the CLI auto-discovers
// a coordinator. Short — operators waiting at a TTY expect a snappy
// "not found, fallback" path on LANs without a coordinator.
const mdnsDiscoveryTimeout = 2 * time.Second

// newClusterPairCmd wires `zzrouter cluster pair`. Worker-side entry
// point for the pairing flow: generates a code, prints it, and leaves
// the node's own pairing loop running until the operator accepts on
// the coord (or the 15-min window elapses).
//
// Coordinator URL + CA fingerprint are pairing-time inputs. Resolution
// order:
//  1. --coordinator-url + --ca-fingerprint flags when both supplied.
//  2. mDNS browse for a coordinator advertising ca_fingerprint +
//     cluster_port (suppressed with --no-mdns).
//  3. Explicit "nothing available" error pointing operators at the
//     flags or at enabling mDNS.
//
// Cancel / regenerate / reuse-of-active-window paths skip resolution.
func newClusterPairCmd() *cobra.Command {
	var regenerate, cancel, noMDNS, noTUI, secure, reset bool
	var coordURL, caFingerprint string
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Enter pairing mode and print the code for the coordinator operator",
		Long: `Enter pairing mode on this worker.

Generates an 80-bit pairing code, persists it to pairing.txt, and
starts a polling loop against the coordinator. The operator reads the
printed code and runs 'zzrouter cluster accept <code>' on the
coordinator to complete the handshake.

Supply the coordinator via --coordinator-url and --ca-fingerprint, or
let mDNS auto-discover one on the LAN. Use --no-mdns to skip browsing.
Idempotent: if a pairing window is already active, prints the existing
code and remaining TTL. Use --regenerate to cancel the current window
and start a fresh one, or --cancel to close without pairing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if regenerate && cancel {
				return fmt.Errorf("--regenerate and --cancel are mutually exclusive")
			}
			// Note: the --secure / --ca-fingerprint consistency
			// check lives inside resolveCoordinatorInputs so the
			// same rule applies whether the operator supplied
			// both up-front or only the URL (with mDNS discovery
			// in between).
			return runClusterPair(regenerate, cancel, noMDNS, noTUI, secure, reset, coordURL, caFingerprint)
		},
	}
	cmd.Flags().BoolVar(&regenerate, "regenerate", false, "Cancel the active window and start a fresh one")
	cmd.Flags().BoolVar(&cancel, "cancel", false, "Cancel the active pairing window without pairing")
	cmd.Flags().BoolVar(&noMDNS, "no-mdns", false, "Skip mDNS auto-discovery even when flags are empty")
	cmd.Flags().BoolVar(&noTUI, "no-tui", false, "Force plain-text output instead of the interactive wizard (automatic when stdout is not a TTY)")
	cmd.Flags().BoolVar(&secure, "secure", false, "Require CA fingerprint pinning on the bootstrap handshake (default: TOFU: trust the CA the coordinator hands back)")
	cmd.Flags().BoolVar(&reset, "reset", false, "If the node is already paired, wipe the current cluster trust and re-pair (destructive: current mTLS CA is discarded)")
	cmd.Flags().StringVar(&coordURL, "coordinator-url", "", "Coordinator cluster-port URL (e.g. https://coord:9091)")
	cmd.Flags().StringVar(&caFingerprint, "ca-fingerprint", "", "Coordinator CA SPKI fingerprint (sha256:<hex> or bare hex); optional unless --secure")
	return cmd
}

func runClusterPair(regenerate, cancel, noMDNS, noTUI, secure, reset bool, coordURL, caFingerprint string) error {
	cm := pkgConfig.NewConfigManager("zzrouter")
	cfg, err := cm.LoadNodeConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cancel {
		// Both refusals name the file they judged. A node can have
		// more than one node.yaml (the operator's and the service
		// account's), and "this node is a coordinator" is unactionable
		// until you know which of them said so.
		switch cfg.Cluster.Mode {
		case pkgConfig.ClusterModeCoordinator:
			return fmt.Errorf("this node is a coordinator, and 'cluster pair' only runs on workers.\n  Config read: %s\n  To repurpose this node as a worker, set cluster.mode: worker there, restart zzrouter-node, then rerun 'cluster pair'", cfg.SourcePath)
		case pkgConfig.ClusterModeDisabled, pkgConfig.ClusterModeStandalone:
			return fmt.Errorf("clustering is disabled on this node.\n  Config read: %s\n  Set cluster.mode: worker there first", cfg.SourcePath)
		}
	}
	// Normalize a flag-supplied URL early so operators who omit
	// the scheme or port (e.g. "198.51.100.180" or "coord.local") get
	// the default cluster port + https:// prepended rather than
	// dying at preflight with a port-80 refusal or a TLS handshake
	// error.
	if coordURL != "" {
		normalized, nerr := pair.NormalizeCoordinatorURL(coordURL)
		if nerr != nil {
			return fmt.Errorf("--coordinator-url: %w", nerr)
		}
		if normalized != coordURL {
			fmt.Fprintf(os.Stderr, "Note: normalized --coordinator-url to %s\n", normalized)
		}
		coordURL = normalized
	}
	localPort := cfg.Node.Port
	if localPort == 0 {
		localPort = constants.DefaultZZROUTERPort
	}
	adminKey, _ := cfg.Auth.GetAdminAPIKey()

	// --reset: if the node is already paired, wipe the current
	// cluster trust before attempting to pair. Harmless on an
	// Unclaimed node (reset endpoint 400s with "not in worker mode"
	// which we swallow). Applies to both TUI and plain-output
	// paths — destructive consent already expressed by the flag.
	if reset {
		if err := callResetEndpoint(localPort, adminKey); err != nil {
			return fmt.Errorf("--reset: %w", err)
		}
	}

	// TUI path: interactive, no --cancel/--regenerate, TTY on stdout.
	// Falls through to plain-output when any condition fails, so the
	// existing scripted callers + --cancel / --regenerate flows are
	// unchanged.
	if !noTUI && !cancel && !regenerate && isInteractive() {
		return runClusterPairTUI(cfg, localPort, adminKey, noMDNS, secure, coordURL, caFingerprint)
	}

	resolvedURL, resolvedFP, err := resolveCoordinatorInputs(cfg, noMDNS, cancel, regenerate, secure, coordURL, caFingerprint)
	if err != nil {
		return err
	}
	coordURL, caFingerprint = resolvedURL, resolvedFP

	body, err := json.Marshal(map[string]any{
		"regenerate":                 regenerate,
		"cancel":                     cancel,
		"coordinator_url":            coordURL,
		"coordinator_ca_fingerprint": caFingerprint,
	})
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/cluster/pair", localPort)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}

	client := &http.Client{Timeout: constants.ClusterActionTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("local server unreachable (is it running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return errors.New(pair.FriendlyPairHandlerError(resp.StatusCode, respBody))
	}

	var out struct {
		Status   string    `json:"status"`
		Code     string    `json:"code"`
		Deadline time.Time `json:"deadline"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	switch out.Status {
	case "cancelled":
		fmt.Println("Pairing cancelled.")
	case "no_active_window":
		fmt.Println("No active pairing window.")
	case "existing":
		remaining := time.Until(out.Deadline).Round(time.Second)
		fmt.Printf("Pairing already active: %s (valid for %s more).\n",
			formatPairingCode(out.Code), remaining)
		fmt.Println("Use --regenerate for a fresh code, or --cancel to close the window.")
	case "new":
		fmt.Printf("Pairing code: %s (valid until %s).\n",
			formatPairingCode(out.Code),
			out.Deadline.UTC().Format(time.RFC3339))
		fmt.Printf("Run 'zzrouter cluster accept %s' on the coordinator.\n",
			formatPairingCode(out.Code))
	default:
		fmt.Printf("Pairing status: %s\n", out.Status)
	}
	return nil
}

// discoverCoordinator runs a short mDNS browse for a coordinator
// advertising ca_fingerprint + cluster_port. A coordinator browsing
// for itself would pick its own record — shouldn't happen in practice
// (coord-mode doesn't run `cluster pair`), but gate defensively via
// the selfIsCoordinator flag so the helper stays safe to call from
// either role.
func discoverCoordinator(selfIsCoordinator bool) (*network.NodeEntry, error) {
	if selfIsCoordinator {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), mdnsDiscoveryTimeout)
	defer cancel()
	hd := network.NewNodeDiscovery("", false, 0)
	return hd.FindCoordinator(ctx)
}

// shortFingerprint returns a compact form for operator display.
// "sha256:abc…def" — first 8 + last 4 of the hex payload.
func shortFingerprint(fp string) string {
	if len(fp) < 20 {
		return fp
	}
	// Strip optional sha256: prefix for the short rendering, then re-add.
	prefix := ""
	hex := fp
	if strings.HasPrefix(fp, "sha256:") {
		prefix = "sha256:"
		hex = fp[len("sha256:"):]
	}
	if len(hex) < 16 {
		return fp
	}
	return prefix + hex[:8] + "…" + hex[len(hex)-4:]
}

// callResetEndpoint POSTs to the local /zzrouter/v1/cluster/reset
// admin endpoint to revert this node from Worker to Unclaimed. The
// server returns 200 for Worker (revert) and Unclaimed (no-op); any
// other mode yields 400 "not in worker mode" which we surface so the
// operator sees the real failure instead of a mystery pair 400 a
// moment later.
func callResetEndpoint(localPort int, adminKey string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/cluster/reset", localPort)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("build reset request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if adminKey != "" {
		req.Header.Set("X-API-Key", adminKey)
	}
	httpClient := &http.Client{Timeout: constants.ClusterActionTimeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("local server unreachable (is it running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return errors.New(pair.FriendlyPairHandlerError(resp.StatusCode, body))
	}
	fmt.Println("Reset: local cluster trust wiped; proceeding with pair.")
	return nil
}

// resolveCoordinatorInputs fills in missing URL / fingerprint from
// mDNS and applies the TOFU / --secure policy. Extracted from
// runClusterPair to keep that function under the gocyclo cap.
func resolveCoordinatorInputs(cfg *pkgConfig.NodeConfig, noMDNS, cancel, regenerate, secure bool, coordURL, caFingerprint string) (string, string, error) {
	needsResolve := !cancel && coordURL == ""
	if needsResolve && !noMDNS && cfg.MDNSDiscovery {
		entry, derr := discoverCoordinator(cfg.Cluster.IsCoordinator())
		if derr != nil {
			fmt.Fprintf(os.Stderr, "mDNS discovery failed: %v\n", derr)
		} else if entry != nil {
			coordURL = entry.PairingURL()
			// mDNS supplies the URL only; the CA fingerprint always
			// rides an OOB channel (flag or interactive prompt).
			if caFingerprint != "" {
				fmt.Printf("Discovered coordinator %s at %s (pinned to supplied fingerprint).\n",
					entry.Name, coordURL)
			} else {
				fmt.Printf("Discovered coordinator %s at %s.\n",
					entry.Name, coordURL)
			}
		}
	}
	if needsResolve && coordURL == "" {
		return "", "", errors.New(
			"no coordinator configured: supply --coordinator-url, " +
				"or enable mDNS (mdns_discovery: true in node.yaml) and ensure a coordinator is broadcasting on the LAN")
	}
	if secure && caFingerprint == "" {
		return "", "", errors.New("--secure requires a CA fingerprint (flag or mDNS-advertised)")
	}
	// Preflight: ask the coord whether it requires secure pairing
	// before we open a window. Skipped when a pin is already in hand
	// (nothing to learn) and on cancel/regenerate (not a fresh
	// window). Preflight failure is fail-closed: without the
	// response we can't tell whether the coord enforces secure
	// pairing, so continuing in TOFU risks silently bypassing a
	// strict-mode coord via MITM. Operators supply --ca-fingerprint
	// to skip preflight entirely.
	if !cancel && !regenerate && coordURL != "" && caFingerprint == "" {
		preflightCtx, preflightCancel := context.WithTimeout(context.Background(), 5*time.Second)
		policy, perr := clusternode.FetchPairingPolicy(preflightCtx, coordURL)
		preflightCancel()
		switch {
		case perr != nil:
			return "", "", fmt.Errorf(
				"%s\n  Fix: start the coordinator, or re-run with --ca-fingerprint <fp> (verified out-of-band) to skip preflight",
				pair.FriendlyPreflightError(coordURL, perr))
		case policy.RequireSecurePairing:
			return "", "", fmt.Errorf(
				"coordinator requires secure pairing; re-run with --ca-fingerprint %s (verify this matches the coordinator's CA out-of-band before trusting)",
				policy.Fingerprint)
		}
	}
	if !cancel && !regenerate && caFingerprint == "" {
		fmt.Fprintln(os.Stderr, "WARNING: pairing in TOFU mode. On any untrusted network an active MITM can pin their own CA into this worker's trust store permanently: this is strictly weaker than SSH TOFU. Only safe on a trusted LAN. Use --secure + --ca-fingerprint (verified out-of-band) on anything else.")
	}
	return coordURL, caFingerprint, nil
}

// isInteractive reports whether both stdin and stdout are connected
// to a terminal. The TUI pairing wizard requires stdin for key events
// and a real stdout so ANSI escapes don't leak into piped consumers.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runClusterPairTUI dispatches the interactive pairing wizard. The
// wizard handles mDNS discovery + operator-confirmation internally
// (closing the gap the plain-output path has, where the first mDNS
// hit is accepted silently).
func runClusterPairTUI(cfg *pkgConfig.NodeConfig, localPort int, adminKey string, noMDNS, secure bool, coordURL, caFingerprint string) error {
	// A coordinator mistakenly running `cluster pair` must not browse
	// its own mDNS record; defensive skip mirrors the plain-output
	// path (discoverCoordinator(selfIsCoordinator=true)).
	selfIsCoord := cfg.Cluster.IsCoordinator()
	effectiveNoMDNS := noMDNS || !cfg.MDNSDiscovery || selfIsCoord

	// Theme resolution: env var wins over config; falls back to auto.
	theme := ui.ResolveTheme("auto")
	if env := os.Getenv("ZZROUTER_THEME"); env != "" {
		theme = ui.ResolveTheme(env)
	} else if clientCfg, err := pkgConfig.NewConfigManager("zzrouter").LoadClientConfig(); err == nil && clientCfg.Preferences.Theme != "" {
		theme = ui.ResolveTheme(clientCfg.Preferences.Theme)
	}

	m := pair.New(pair.Config{
		CoordURL:      coordURL,
		CAFingerprint: caFingerprint,
		NoMDNS:        effectiveNoMDNS,
		Secure:        secure,
		LocalPort:     localPort,
		AdminAPIKey:   adminKey,
		Theme:         theme,
	})

	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	if mm, ok := finalModel.(*pair.Model); ok {
		res := mm.Result()
		if !res.Success && res.FailReason != "" {
			return errors.New(res.FailReason)
		}
	}
	return nil
}

// formatPairingCode inserts a dash every 4 characters for operator
// readability: "ABCDEFGHIJKLMNOP" → "ABCD-EFGH-IJKL-MNOP". The server
// accepts either form (dash-stripping happens in the admin handler).
func formatPairingCode(code string) string {
	if len(code) != 16 {
		return code
	}
	return code[0:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16]
}
