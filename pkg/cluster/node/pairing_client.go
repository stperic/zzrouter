package clusternode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	clusterid "github.com/stperic/zzrouter/pkg/cluster/id"
	"github.com/stperic/zzrouter/pkg/retry"
	"github.com/stperic/zzrouter/pkg/version"
)

// pairingBackoff is the transport-error retry policy for the pairing
// poll loop. Initial cadence matches pollIntervalDefault; jitter
// prevents synchronized retry waves after a coord restart.
var pairingBackoff = retry.Backoff{
	Initial:    pollIntervalDefault,
	Max:        pollBackoffCap,
	Multiplier: pairingBackoffMultiplier,
	Jitter:     pairingBackoffJitter,
}

// Sentinel errors for the worker-side pairing loop. Callers match via
// errors.Is so CLI + lifecycle code can branch on kind without
// string-scraping.
var (
	// ErrPairingWindowExpired fires when the coord responded expired.
	// The worker's 15-min timer will also fire on the same signal; the
	// client returns first whichever path closes the window.
	ErrPairingWindowExpired = errors.New("clusternode: pairing window expired")

	// ErrPairingBadCA fires when the coord-returned CA cert does not
	// match the worker's configured CoordinatorCAFingerprint. Defense-
	// in-depth: the transport already pinned on this value, but a bug
	// in the TLS stack or a misconfigured tls.Config could otherwise
	// slip a wrong cert through.
	ErrPairingBadCA = errors.New("clusternode: ca_cert_pem does not match configured CA fingerprint")

	// ErrPairingCAFingerprintMalformed fires on a config-time error:
	// the supplied hex pin doesn't decode. Surfaced at newCAPinnedClient
	// construction so BeginPairing fails at invocation, not at first
	// handshake.
	ErrPairingCAFingerprintMalformed = errors.New("clusternode: coordinator_ca_fingerprint is not valid hex")

	// ErrPairingStrictModeRequired fires when the coordinator returns
	// 403 on a pair request because it has require_secure_pairing=true
	// and the worker did not echo a matching fingerprint. Terminal —
	// the loop stops retrying and surfaces this up so the operator
	// can re-run with --ca-fingerprint rather than waiting for the
	// 15-minute window to expire.
	ErrPairingStrictModeRequired = errors.New("clusternode: coordinator requires secure pairing (HTTP 403 on pair request)")
)

// pollIntervalDefault is the fallback poll cadence used when the coord
// response doesn't carry poll_interval_seconds. Matches the 3s value
// the coord hands back in waiting_for_operator responses today.
const pollIntervalDefault = 3 * time.Second

// pollBackoffCap bounds transport-error backoff so a flapping coord
// doesn't leave the worker polling once a minute forever.
const pollBackoffCap = 60 * time.Second

// pairingHTTPTimeout is the outer HTTP client deadline — covers the
// coord's 30s server-side hold plus network slack. Set well above the
// server hold so a slow TLS handshake doesn't kill the long-poll.
const pairingHTTPTimeout = 60 * time.Second

// pairingBackoff policy knobs. Extracted so tuning lands in one place
// rather than inside the var literal.
const (
	// pairingBackoffMultiplier doubles transport-error backoff per
	// consecutive failure, until Max (pollBackoffCap) is hit.
	pairingBackoffMultiplier = 2.0
	// pairingBackoffJitter is ±20% of the computed delay so fleets of
	// paired workers don't retry in lockstep after a coord restart.
	pairingBackoffJitter = 0.2
)

// newPairingClient returns the HTTP client the worker uses to reach
// the coordinator during the pairing bootstrap. When caFingerprint is
// empty the handshake runs in TOFU mode (InsecureSkipVerify) and the
// coordinator's CA is pinned on approval via the returned CACertPEM.
// When non-empty the fingerprint is pinned on the initial handshake
// (see newCAPinnedClient).
//
// TOFU is the low-friction home-lab default; strict mode is opt-in
// via the caller's --secure flag.
func newPairingClient(caFingerprint string) (*http.Client, error) {
	if caFingerprint == "" {
		return newInsecurePairingClient(), nil
	}
	return newCAPinnedClient(caFingerprint)
}

// newInsecurePairingClient returns a TOFU-mode client: no chain
// validation on the bootstrap handshake. Suitable ONLY for trusted
// LANs. On approval the worker binds the returned CA to the TLS leaf
// it observed (verifyCAPEMSignsLeaf) so a passive response-swap
// attack fails, but an active MITM who terminates TLS and serves a
// fully self-consistent chain (their own CA + leaf signed by it)
// still passes — and their CA then gets persisted as the worker's
// permanent cluster trust root. That's strictly weaker than SSH
// TOFU (which binds to the host key on the wire); on any untrusted
// network operators must use --secure + an OOB-verified fingerprint.
func newInsecurePairingClient() *http.Client {
	tlsCfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		InsecureSkipVerify:     true, //nolint:gosec // TOFU bootstrap; --secure opts into fingerprint pinning
		SessionTicketsDisabled: true,
	}
	return &http.Client{
		Timeout: pairingHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// newCAPinnedClient returns an HTTP client that verifies peer
// certificates against a single SPKI SHA256 pin. Suitable for the
// pre-pairing leg: the worker doesn't have the CA cert yet, but does
// have a hash of the CA's public key.
//
// How the pin is enforced:
//
//   - tls.Config.InsecureSkipVerify: true, which disables the stdlib's
//     default chain validation. This is the only way to install a
//     custom verification hook that doesn't require an already-trusted
//     root pool.
//   - VerifyPeerCertificate parses the presented chain, locates a
//     self-signed cert whose SPKI matches our pin, then runs a real
//     x509.Verify against that root so NotBefore/NotAfter + signature
//     integrity are checked. A chain without a matching root fails.
//
// Without the explicit Verify-against-root step, a cert that merely
// CLAIMED to chain to a root with the right SPKI would pass — we must
// actually validate the chain.
func newCAPinnedClient(caFingerprint string) (*http.Client, error) {
	pin, err := normalizeCAFingerprint(caFingerprint)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // verification moved to VerifyPeerCertificate (CA fingerprint pinning); default chain trust isn't used
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyAgainstPinnedCA(rawCerts, pin)
		},
		SessionTicketsDisabled: true,
	}
	return &http.Client{
		Timeout: pairingHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

// normalizeCAFingerprint is the pairing-client-specific wrapper for
// clusterid.NormalizeFingerprint. Delegates to the shared normalizer
// so the format semantics (length, prefix handling, case folding)
// stay in lock-step with every other fingerprint validator in the
// cluster subsystem; wraps the underlying error in
// ErrPairingCAFingerprintMalformed so BeginPairing's config-validation
// error surface is stable.
func normalizeCAFingerprint(s string) (string, error) {
	hexStr, err := clusterid.NormalizeFingerprint(s)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrPairingCAFingerprintMalformed, err)
	}
	return hexStr, nil
}

// preflightPolicyTimeout caps the pre-pair policy probe. Short —
// operators at a TTY expect snappy feedback and the endpoint is a
// single GET on the coord's cluster port.
const preflightPolicyTimeout = 3 * time.Second

// PairingPolicy is the decoded result of a preflight GET against
// /cluster/ca-fingerprint. Shared by the CLI and TUI wizard so the
// HTTP client + timeout + JSON shape live in one place.
type PairingPolicy struct {
	Fingerprint          string
	RequireSecurePairing bool
}

// FetchPairingPolicy probes the coordinator's CA-fingerprint
// endpoint over insecure TLS to learn whether it requires secure
// pairing. The response is operator-facing only: the fingerprint it
// returns rode an unauthenticated handshake, so callers treat it as
// a UX hint, not an OOB reference. Transport / decode errors propagate
// — callers are expected to fail-closed rather than silently degrade
// to TOFU.
func FetchPairingPolicy(ctx context.Context, coordURL string) (*PairingPolicy, error) {
	if coordURL == "" {
		return nil, errors.New("preflight: empty coordinator url")
	}
	client := &http.Client{
		Timeout: preflightPolicyTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS13,
				InsecureSkipVerify: true, //nolint:gosec // public fingerprint endpoint; policy bit is a UX hint, not a trust boundary
			},
		},
	}
	url := strings.TrimRight(coordURL, "/") + "/zzrouter/v1/cluster/ca-fingerprint"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("preflight: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("preflight: reach coordinator: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("preflight: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Fingerprint          string `json:"fingerprint"`
		RequireSecurePairing bool   `json:"require_secure_pairing"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("preflight: decode: %w", err)
	}
	return &PairingPolicy{
		Fingerprint:          out.Fingerprint,
		RequireSecurePairing: out.RequireSecurePairing,
	}, nil
}

// verifyCAPEMSignsLeaf binds an approval-time CA to the TLS leaf
// observed during the handshake. Prevents a passive on-path attacker
// from swapping CACertPEM in the JSON body while relaying the real
// TLS session — the attacker's fake CA won't sign the real coord's
// leaf. Does NOT defeat an active MITM with TLS termination; that
// threat model requires an OOB fingerprint (--secure).
//
// Beyond the chain check, enforces CA-sanity on the returned cert:
//   - BasicConstraints present + IsCA true (rejects leaf-shaped
//     certs that happen to have signed the TLS leaf)
//   - KeyUsage includes CertSign (rejects certs whose key usage
//     restrictions make them invalid as issuers going forward —
//     which matters because this CA is persisted as the cluster
//     trust root and must be able to validate future worker certs).
func verifyCAPEMSignsLeaf(caPEM []byte, leaf *x509.Certificate) error {
	if leaf == nil {
		return errors.New("tofu verify: no TLS leaf captured from handshake")
	}
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return errors.New("tofu verify: ca_cert_pem is not valid PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("tofu verify: parse ca_cert_pem: %w", err)
	}
	if !caCert.BasicConstraintsValid || !caCert.IsCA {
		return errors.New("tofu verify: ca_cert_pem is not a CA (BasicConstraints / IsCA)")
	}
	if caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("tofu verify: ca_cert_pem KeyUsage lacks CertSign: cannot serve as cluster trust root")
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		return fmt.Errorf("tofu verify: returned CA does not chain the TLS handshake leaf: %w", err)
	}
	return nil
}

// verifyAgainstPinnedCA is the VerifyPeerCertificate callback body.
// Parses the chain, locates a self-signed root whose SPKI matches the
// pin, then runs a real x509.Verify against a pool containing only
// that root.
func verifyAgainstPinnedCA(rawCerts [][]byte, pinHex string) error {
	if len(rawCerts) == 0 {
		return errors.New("peer presented no certificates")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for i, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse cert %d: %w", i, err)
		}
		certs = append(certs, c)
	}

	// Locate a root whose SPKI matches the pin. Usually last in the
	// chain; walk all entries defensively (some servers send the root
	// first or mid-chain).
	var root *x509.Certificate
	for _, c := range certs {
		if spkiMatches(c, pinHex) {
			root = c
			break
		}
	}
	if root == nil {
		return fmt.Errorf("%w: no cert in presented chain matches pin %s…", ErrPairingBadCA, pinHex[:8])
	}

	leaf := certs[0]
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for i := 1; i < len(certs); i++ {
		if certs[i] != root {
			intermediates.AddCert(certs[i])
		}
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("chain verify: %w", err)
	}
	return nil
}

// spkiMatches reports whether cert's SubjectPublicKeyInfo SHA256 equals
// the given lowercase hex pin.
func spkiMatches(cert *x509.Certificate, pinHex string) bool {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]) == pinHex
}

// verifyCAPEMMatchesPin confirms the CA cert returned in the pairing
// response has the same SPKI as the pin. Belt-and-suspenders against a
// TLS-layer verifier bug — we already pinned the transport, but if the
// returned ca_cert_pem differs from what served the TLS session, we
// want to know before persisting it.
func verifyCAPEMMatchesPin(caPEM []byte, pinHex string) error {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("returned ca_cert_pem is not a CERTIFICATE PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse returned CA: %w", err)
	}
	if !spkiMatches(cert, pinHex) {
		return ErrPairingBadCA
	}
	return nil
}

// pairingClientOpts carries everything a running pairing loop needs.
// Separate from Node so the tests can drive the loop without building
// a full coord + identity + listener.
type pairingClientOpts struct {
	coordURL       string
	caFingerprint  string // normalized hex
	code           string
	nodeName       string
	fingerprint    string
	csrPEM         string
	sans           []string
	pollInterval   time.Duration // zero → use poll hint or default
	backoffInitial time.Duration // zero → pollIntervalDefault
	httpClient     *http.Client  // test injection; production uses newCAPinnedClient
}

// pairingLoopResult is what the loop hands back to completePairing on
// success. Value-typed, no pointer into the HTTP response.
type pairingLoopResult struct {
	signedCertPEM  []byte
	caCertPEM      []byte
	coordinatorURL string
}

// runPairingLoop drives a single pairing attempt end-to-end. Returns
// on success, on expiry, or on ctx cancel. Transport errors trigger
// exponential backoff.
func runPairingLoop(ctx context.Context, opts pairingClientOpts) (*pairingLoopResult, error) {
	if opts.httpClient == nil {
		c, err := newPairingClient(opts.caFingerprint)
		if err != nil {
			return nil, err
		}
		opts.httpClient = c
	}
	// Empty fingerprint → TOFU: no pin to verify CACertPEM against on
	// approval. Instead, the approval path binds the returned CA to
	// the TLS leaf we observed on the handshake, so a passive relayer
	// can't swap the CA in the response body. Non-empty → defense-
	// in-depth check even though the transport already pinned the
	// handshake.
	var pin string
	if opts.caFingerprint != "" {
		p, err := normalizeCAFingerprint(opts.caFingerprint)
		if err != nil {
			return nil, err
		}
		pin = p
	}

	// transportFails counts consecutive transport errors. Reset on any
	// valid coord response; feeds pairingBackoff.Delay for the next wait.
	transportFails := 0

	// Allow tests to override the policy via opts.backoffInitial.
	backoff := pairingBackoff
	if opts.backoffInitial > 0 {
		backoff.Initial = opts.backoffInitial
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, pollHint, leaf, err := submitPairingRequest(ctx, opts)
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			// Strict-mode rejection is terminal — the coord will
			// keep returning 403 for every retry. Surface the
			// sentinel up so the operator learns to re-run with
			// --ca-fingerprint instead of waiting for the window
			// to expire.
			if errors.Is(err, ErrPairingStrictModeRequired) {
				return nil, err
			}
			// Transport-level failure. Sleep jittered backoff, bump counter.
			if err := retry.Sleep(ctx, backoff.Delay(transportFails)); err != nil {
				return nil, err
			}
			transportFails++
			continue
		}
		// Any valid response resets backoff — the coord is reachable.
		transportFails = 0

		switch resp.Status {
		case "approved":
			if pin != "" {
				// Defense-in-depth re-check of the returned CA cert
				// against the caller-supplied pin.
				if err := verifyCAPEMMatchesPin([]byte(resp.CACertPEM), pin); err != nil {
					return nil, err
				}
			} else {
				// TOFU: bind the returned CA to the TLS leaf we
				// observed on the handshake, so a relayer can't
				// simply swap CACertPEM in the response body.
				if err := verifyCAPEMSignsLeaf([]byte(resp.CACertPEM), leaf); err != nil {
					return nil, err
				}
			}
			url := resp.CoordinatorURL
			if url == "" {
				url = opts.coordURL
			}
			return &pairingLoopResult{
				signedCertPEM:  []byte(resp.SignedCertPEM),
				caCertPEM:      []byte(resp.CACertPEM),
				coordinatorURL: url,
			}, nil

		case "expired":
			return nil, ErrPairingWindowExpired

		case "waiting_for_operator":
			wait := opts.pollInterval
			if wait <= 0 {
				if pollHint > 0 {
					wait = time.Duration(pollHint) * time.Second
				} else {
					wait = pollIntervalDefault
				}
			}
			if err := retry.Sleep(ctx, wait); err != nil {
				return nil, err
			}

		default:
			// Unknown status — treat as transient, back off.
			if err := retry.Sleep(ctx, backoff.Delay(transportFails)); err != nil {
				return nil, err
			}
			transportFails++
		}
	}
}

// pairingPollResponse mirrors the coord's handler response shape. A
// single struct because the three status values each populate a
// disjoint subset of fields.
type pairingPollResponse struct {
	Status                  string `json:"status"`
	PollIntervalSeconds     int    `json:"poll_interval_seconds,omitempty"`
	SignedCertPEM           string `json:"signed_cert_pem,omitempty"`
	CACertPEM               string `json:"ca_cert_pem,omitempty"`
	CoordinatorURL          string `json:"coordinator_url,omitempty"`
	ClusterProtocol         int    `json:"cluster_protocol,omitempty"`
	MinClusterProtocol      int    `json:"min_cluster_protocol,omitempty"`
	CoordinatorBuildVersion string `json:"coordinator_build_version,omitempty"`
}

// submitPairingRequest executes one long-poll POST. Returns the
// decoded response + a poll-interval hint from the response body +
// the TLS leaf cert observed on the handshake (used by TOFU to bind
// the approval-time CA to the session; nil when the request was
// served over plain HTTP or before handshake). A non-2xx coord
// response maps to a transport-style error so the loop treats it as
// transient (per design doc: only the three status strings are
// terminal).
func submitPairingRequest(ctx context.Context, opts pairingClientOpts) (*pairingPollResponse, int, *x509.Certificate, error) {
	body, err := json.Marshal(map[string]any{
		"node_name":                  opts.nodeName,
		"fingerprint":                opts.fingerprint,
		"csr_pem":                    opts.csrPEM,
		"sans":                       opts.sans,
		"code":                       opts.code,
		"cluster_protocol":           version.ClusterProtocolVersion,
		"min_cluster_protocol":       version.MinClusterProtocolVersion,
		"build_version":              version.Current.String(),
		"coordinator_ca_fingerprint": opts.caFingerprint, // echoed so coord can gate on it when accept_insecure_pairing=false
	})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(opts.coordURL, "/")+"/zzrouter/v1/cluster/pairing-request",
		bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := opts.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	// 403 is the coord's signal that accept_insecure_pairing=false
	// and this worker didn't echo a matching fingerprint. Terminal —
	// retry won't help — so return a sentinel the loop can break on
	// rather than the generic transport-error path.
	if resp.StatusCode == http.StatusForbidden {
		return nil, 0, nil, fmt.Errorf("%w: %s", ErrPairingStrictModeRequired, strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, nil, fmt.Errorf("pairing-request: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out pairingPollResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, 0, nil, fmt.Errorf("decode response: %w", err)
	}
	var leaf *x509.Certificate
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		leaf = resp.TLS.PeerCertificates[0]
	}
	return &out, out.PollIntervalSeconds, leaf, nil
}
