// Package pair implements the Bubbletea v2 pairing wizard used by
// `zzrouter cluster pair` when invoked from an interactive terminal.
//
// The wizard discovers a coordinator via mDNS, asks the operator to
// confirm the discovered identity, opens a pairing window on the local
// node, and polls status at 1 Hz until the coordinator accepts the
// code (success), the window expires, or the operator cancels.
//
// Model is exported so an install wizard can embed it as one sub-state
// of its own state machine.
package pair

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/utils"
)

// PairRequest mirrors the JSON body the local pair handler accepts.
// Declared here to keep this package free of internal/server imports;
// TestContractMatchesHandler in pair_test.go pins the tag-level shape
// against internal/server.clusterPairRequest so drift fails loudly.
type PairRequest struct {
	Regenerate               bool   `json:"regenerate"`
	Cancel                   bool   `json:"cancel"`
	CoordinatorURL           string `json:"coordinator_url,omitempty"`
	CoordinatorCAFingerprint string `json:"coordinator_ca_fingerprint,omitempty"`
}

// PairResponse mirrors the JSON response from the local pair handler.
// Status values: "new" | "existing" | "cancelled" | "no_active_window".
// Error is set (when the handler can determine it) to the terminal
// failure reason from the background pairing loop — e.g. the coord
// returned 403 because require_secure_pairing is set and the worker
// didn't echo a matching fingerprint.
type PairResponse struct {
	Status   string    `json:"status"`
	Code     string    `json:"code,omitempty"`
	Deadline time.Time `json:"deadline,omitempty"`
	Error    string    `json:"error,omitempty"`
	// Mode is the node's runtime cluster mode. On a no_active_window
	// response, Mode == "worker" is the authoritative success signal:
	// the coord consumed the window and the local lifecycle flipped.
	Mode string `json:"mode,omitempty"`
}

// PairResult is what the embedding caller (C5 install wizard, or a
// non-interactive summary) can read after the wizard quits.
type PairResult struct {
	Success       bool
	Code          string
	CoordURL      string
	CAFingerprint string
	// FailReason is set when Success is false; empty otherwise.
	FailReason string
}

// DiscoverFunc is the dependency for mDNS discovery; injection hook
// for tests.
type DiscoverFunc func(ctx context.Context) (*network.NodeEntry, error)

// PairPostFunc is the dependency for POST /zzrouter/v1/cluster/pair;
// injection hook for tests.
type PairPostFunc func(ctx context.Context, req PairRequest) (PairResponse, error)

// ClipboardFunc is the dependency for copy-to-clipboard; injection
// hook for tests and for SSH environments where clipboard is absent.
type ClipboardFunc func(string) error

// Config bundles inputs and injection hooks for the Model. All
// injection hooks are optional — zero values fall back to real impls.
type Config struct {
	CoordURL         string
	CAFingerprint    string
	NoMDNS           bool
	Secure           bool // when true, CAFingerprint is required; otherwise pairing runs in TOFU mode
	LocalPort        int
	AdminAPIKey      string
	DiscoveryTimeout time.Duration
	Theme            ui.Theme // optional; defaults to ResolveTheme("auto")

	// Injection hooks (nil → real impls).
	Discover        DiscoverFunc
	PairPost        PairPostFunc
	Clock           func() time.Time
	Clipboard       ClipboardFunc
	TransferFileDir string // default: os.UserStateDir()/zzrouter

	// HTTPClient is used by the default PairPost impl; unused when
	// PairPost is injected.
	HTTPClient *http.Client
}

// Model is the Bubbletea v2 tea.Model for the pairing wizard.
type Model struct {
	cfg     Config
	styles  ui.Styles
	state   state
	spinner int // frame counter for the shared braille-dot spinner

	// Discovered / confirmed coordinator identity.
	coordURL      string
	caFingerprint string
	coordName     string // mDNS advertised name; "" for flag-supplied

	// Active pairing window state.
	code       string
	deadline   time.Time
	seenActive bool // true once we've observed status="existing" at least once
	reqGen     int  // bumped on each request-issuing transition; pins poll responses to the window that produced them

	// Transient UX state.
	flash      string    // one-line ephemeral status ("code copied", etc.)
	flashUntil time.Time // cleared at this instant
	err        error     // last terminal error (when state == stateFailed)

	// Manual entry inputs (stateManualEntry). Populated lazily the
	// first time the operator lands on the manual-entry form.
	manualURL    textinput.Model
	manualFP     textinput.Model
	manualFocus  int // 0 = URL, 1 = fingerprint
	manualErrMsg string
	manualReady  bool

	// policyInFlight blocks the confirm-screen `y`/`enter` keypress
	// while a preflight is outstanding. Otherwise a fast operator
	// could fire a TOFU pair request and have a late coordPolicyMsg
	// arrive with requireSecure=true after the window already
	// opened — a silent downgrade path.
	policyInFlight bool

	// Result for embedded callers.
	result PairResult
	done   bool

	termWidth  int
	termHeight int
}

// New constructs a Model. DiscoveryTimeout defaults to 2s; Clock
// defaults to time.Now; Clipboard defaults to atotto clipboard.
func New(cfg Config) *Model {
	if cfg.DiscoveryTimeout == 0 {
		cfg.DiscoveryTimeout = 2 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = utils.Now
	}
	theme := cfg.Theme
	if theme.Primary == nil {
		theme = ui.ResolveTheme("auto")
	}
	return &Model{
		cfg:    cfg,
		styles: ui.NewStyles(theme),
		state:  stateDiscovering,
	}
}

// Done reports whether the wizard has reached a terminal state
// (success, expired, failed, or operator-cancelled).
func (m *Model) Done() bool { return m.done }

// Result returns the wizard outcome. Meaningful only after Done()
// returns true.
func (m *Model) Result() PairResult { return m.result }

// Init implements tea.Model. Fires the initial discovery command or
// skips straight to confirm when flags were supplied.
func (m *Model) Init() tea.Cmd {
	if m.cfg.CoordURL != "" {
		// Flag-supplied identity — still confirm before sending.
		// Fingerprint may be empty when not in Secure mode.
		m.coordURL = m.cfg.CoordURL
		m.caFingerprint = m.cfg.CAFingerprint
		m.state = stateConfirmCoordinator
		// Kick preflight only when we don't already have a pin —
		// with a pin set, the coord's policy doesn't change the flow.
		if m.caFingerprint == "" {
			m.policyInFlight = true
			return m.policyCmd(m.coordURL)
		}
		return nil
	}
	if m.cfg.NoMDNS {
		return m.enterManualEntry()
	}
	// Spinner runs during discovery + requesting; tick in parallel
	// with the mDNS browse.
	return tea.Batch(m.discoverCmd(), ui.SpinnerTickCmd())
}

// enterManualEntry lazy-initializes the two text inputs and focuses
// the URL field. Safe to call repeatedly — subsequent invocations
// preserve any in-progress text the operator typed.
func (m *Model) enterManualEntry() tea.Cmd {
	if !m.manualReady {
		m.manualURL = newPairInput("https://coord.local:9091", 256)
		fpPlaceholder := "sha256:… (optional: leave blank for TOFU)"
		if m.cfg.Secure {
			fpPlaceholder = "sha256:… (required with --secure)"
		}
		m.manualFP = newPairInput(fpPlaceholder, 128)
		m.manualReady = true
	}
	m.state = stateManualEntry
	m.manualFocus = 0
	m.manualErrMsg = ""
	return m.manualURL.Focus()
}

func newPairInput(placeholder string, limit int) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = limit
	return ti
}

// FriendlyPairHandlerError converts an RFC 9457 Problem+JSON body
// (or plain text) from the local /zzrouter/v1/cluster/pair endpoint
// into a short, operator-actionable string. Known-detail strings
// get a rewrite with a "fix hint"; unknown details fall through to
// the raw detail text; truly opaque bodies fall through to the
// status code. Exported so CLI and TUI share one wording set.
func FriendlyPairHandlerError(statusCode int, body []byte) string {
	var problem struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	detail := ""
	if err := json.Unmarshal(body, &problem); err == nil && problem.Detail != "" {
		detail = problem.Detail
	} else if len(body) > 0 && len(body) < 200 {
		// Plain-text body, short enough to surface as-is.
		detail = strings.TrimSpace(string(body))
	}
	switch {
	case strings.Contains(detail, "current mode worker"):
		return "this node is already paired as a worker: re-run with --reset to wipe the current cluster trust and pair again"
	case strings.Contains(detail, "current mode coordinator"):
		return "this node is a coordinator: 'cluster pair' only runs on unclaimed workers"
	case strings.Contains(detail, "current mode disabled"):
		return "cluster mode is disabled on this node: enable clustering in node.yaml before pairing"
	case strings.Contains(detail, "pairing requires unclaimed mode"):
		return "this node is not in unclaimed mode: 'cluster pair' runs on an unpaired worker"
	case strings.Contains(detail, "coordinator_url") && strings.Contains(detail, "must be supplied"):
		return "coordinator URL is missing"
	case strings.Contains(detail, "coordinator_ca_fingerprint is not valid hex"):
		return "CA fingerprint is malformed: expected sha256:<hex> or 64 hex characters"
	case strings.Contains(detail, "cluster listener not initialized"):
		return "the local cluster listener isn't running: check 'cluster' config in node.yaml"
	case detail != "":
		return detail
	}
	return fmt.Sprintf("pair endpoint returned HTTP %d", statusCode)
}

// FriendlyPreflightError turns a verbose wrapped preflight error
// into a short, operator-actionable message. Classifies the common
// cases (connection refused, TLS on wrong port, DNS, timeout) and
// falls through to a generic wording otherwise. Exported so both
// the CLI and TUI render the same phrasing.
func FriendlyPreflightError(coordURL string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return fmt.Sprintf("cannot reach coordinator at %s: is zzrouter-node running and listening on the cluster port?", coordURL)
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return fmt.Sprintf("cannot reach coordinator at %s: host unreachable (check the IP / network)", coordURL)
	case strings.Contains(msg, "HTTP request to an HTTPS server"), strings.Contains(msg, "http: server gave HTTP response to HTTPS client"):
		return fmt.Sprintf("cannot reach coordinator at %s: protocol mismatch (TLS expected). Check the port you typed matches the cluster TLS listener.", coordURL)
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "TLS handshake"):
		return fmt.Sprintf("TLS handshake with coordinator at %s failed: is this the mTLS cluster port? (default %d)", coordURL, constants.DefaultClusterPort)
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "lookup"):
		return fmt.Sprintf("cannot resolve coordinator hostname in %s: check DNS / /etc/hosts or use an IP", coordURL)
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "i/o timeout"):
		return fmt.Sprintf("coordinator at %s did not respond in time: check firewall / routing", coordURL)
	case strings.Contains(msg, "status "):
		return fmt.Sprintf("coordinator at %s returned an unexpected response (%s)", coordURL, trimPrefix(msg, "preflight: "))
	}
	return fmt.Sprintf("preflight to %s failed: %s", coordURL, trimPrefix(msg, "preflight: "))
}

func trimPrefix(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}

// NormalizeCoordinatorURL accepts operator-typed forms like
// "198.51.100.180", "http://198.51.100.180", "coord.local:9091" and returns a
// well-formed "https://host:port". Rules:
//
//   - No scheme → "https://" prepended.
//   - "http://" → promoted to "https://" (the cluster port is always
//     mTLS; plain HTTP would fail the handshake — silently correct
//     rather than force the operator to debug a generic TLS error).
//   - No port → DefaultClusterPort appended.
//   - Paths / queries / fragments stripped (the preflight +
//     pairing endpoints are fixed paths; anything else is operator
//     error).
//
// Returns a friendly error when the input is unrecoverable.
// Exported so the CLI can share the identical rules with the TUI.
func NormalizeCoordinatorURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("coordinator URL is required")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Silent upgrade. The cluster port is mTLS regardless; the
		// operator's only "wrong" here is typing http:// and plain
		// HTTP would fail with an opaque TLS handshake error.
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported scheme %q: use https://", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("URL is missing a host")
	}
	if u.Port() == "" {
		u.Host = u.Hostname() + ":" + strconv.Itoa(constants.DefaultClusterPort)
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	return tea.NewView(m.render())
}
