package pair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/discovery/network"
	"github.com/stperic/zzrouter/pkg/ui"
)

// state enumerates the wizard's finite states. Transitions are driven
// by messages produced either by tea keystroke events or by the
// command helpers below.
type state int

const (
	stateDiscovering state = iota
	stateConfirmCoordinator
	stateManualEntry
	stateRequesting
	statePairing
	stateSuccess
	stateExpired
	stateFailed
	stateCancelled
	// stateConfirmRepair is entered when the pair endpoint reports the
	// node is already paired as a worker. A single y/n gates the
	// destructive re-pair (wipe CA + signed cert, then retry pair).
	stateConfirmRepair
)

// errAlreadyPairedWorker is the internal sentinel defaultPairPost
// returns when the server reports "current mode worker". Used to
// pivot to stateConfirmRepair instead of terminating at stateFailed.
var errAlreadyPairedWorker = errors.New("pair: already paired as worker")

// Poll cadence + TTL tick cadence.
const (
	pollInterval = 1 * time.Second
	flashTTL     = 2 * time.Second
)

// --- Messages ---

type discoveredMsg struct {
	entry *network.NodeEntry
	err   error
}

// coordPolicyMsg carries the /cluster/ca-fingerprint preflight result.
// requireSecure drives whether the wizard lets the operator proceed
// without a pinned fingerprint. The advertised fingerprint from the
// response is intentionally dropped — it rode an unauthenticated TLS
// session, so showing it to the operator as an "OOB reference" would
// be self-contradictory.
type coordPolicyMsg struct {
	requireSecure bool
	err           error
}

// pairRespMsg carries the pair-endpoint outcome. The gen field pins
// the response to the request-issuing transition that produced it —
// Update drops pairRespMsgs whose gen is stale (operator pressed
// regenerate while a poll was in flight), preventing a late response
// from clobbering a fresh window's code/deadline.
type pairRespMsg struct {
	gen  int
	resp PairResponse
	err  error
}

type tickMsg time.Time

// flashMsg carries ephemeral status text from a tea.Cmd back through
// Update, so mutation of m.flash stays on the single Update goroutine.
type flashMsg struct{ text string }

// cancelDoneMsg signals that the fire-on-quit cancel POST finished;
// its arrival returns tea.Quit so the process tears down only after
// the server-side window is closed.
type cancelDoneMsg struct{}

// resetDoneMsg carries the outcome of POST /cluster/reset. Nil err →
// retry the pair request with the stored coordinator inputs.
type resetDoneMsg struct{ err error }

// --- Commands (produced by Update; return tea.Cmd) ---

func (m *Model) discoverCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.DiscoveryTimeout)
		defer cancel()
		disc := m.cfg.Discover
		if disc == nil {
			disc = defaultDiscover
		}
		entry, err := disc(ctx)
		return discoveredMsg{entry: entry, err: err}
	}
}

func defaultDiscover(ctx context.Context) (*network.NodeEntry, error) {
	hd := network.NewNodeDiscovery("", false, 0)
	return hd.FindCoordinator(ctx)
}

// policyCmd runs the shared preflight helper in a cmd so the wizard
// learns the pairing policy before opening a window. Fail-closed on
// error: onCoordPolicy routes the error to stateFailed rather than
// silently falling through to TOFU.
func (m *Model) policyCmd(coordURL string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		policy, err := clusternode.FetchPairingPolicy(ctx, coordURL)
		if err != nil {
			return coordPolicyMsg{err: err}
		}
		return coordPolicyMsg{requireSecure: policy.RequireSecurePairing}
	}
}

// pairCmd POSTs to /zzrouter/v1/cluster/pair. Used both to open a new
// window (req carries CoordinatorURL + optional CAFingerprint) and to poll an
// existing one (req is the zero value, relying on the handler's reuse
// short-circuit). gen is the request generation — stamped into the
// response message so Update can drop stale replies when the operator
// regenerates mid-flight.
func (m *Model) pairCmd(gen int, req PairRequest) tea.Cmd {
	post := m.cfg.PairPost
	if post == nil {
		post = m.defaultPairPost
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := post(ctx, req)
		return pairRespMsg{gen: gen, resp: resp, err: err}
	}
}

func (m *Model) defaultPairPost(ctx context.Context, req PairRequest) (PairResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return PairResponse{}, fmt.Errorf("marshal body: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/cluster/pair", m.cfg.LocalPort)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return PairResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if m.cfg.AdminAPIKey != "" {
		httpReq.Header.Set("X-API-Key", m.cfg.AdminAPIKey)
	}
	client := m.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return PairResponse{}, fmt.Errorf("local server unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		friendly := FriendlyPairHandlerError(resp.StatusCode, respBody)
		if strings.Contains(friendly, "already paired as a worker") {
			return PairResponse{}, fmt.Errorf("%w: %s", errAlreadyPairedWorker, friendly)
		}
		return PairResponse{}, errors.New(friendly)
	}
	var out PairResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return PairResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return out, nil
}

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- Update ---

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Clear flash if expired.
	if m.flash != "" && m.cfg.Clock().After(m.flashUntil) {
		m.flash = ""
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth, m.termHeight = msg.Width, msg.Height
		return m, nil

	case discoveredMsg:
		newM, cmd := m.onDiscovered(msg)
		return newM, cmd

	case pairRespMsg:
		return m.onPairResp(msg)

	case tickMsg:
		return m.onTick()

	case flashMsg:
		m.setFlash(msg.text)
		return m, nil

	case cancelDoneMsg:
		return m, tea.Quit

	case coordPolicyMsg:
		return m.onCoordPolicy(msg)

	case resetDoneMsg:
		return m.onResetDone(msg)

	case ui.SpinnerTickMsg:
		m.spinner++
		if m.spinnerActive() {
			return m, ui.SpinnerTickCmd()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	// Forward non-key messages to the focused textinput while the
	// manual-entry form is visible, so cursor blink and friends keep
	// ticking.
	if m.state == stateManualEntry && m.manualReady {
		var cmd tea.Cmd
		if m.manualFocus == 0 {
			m.manualURL, cmd = m.manualURL.Update(msg)
		} else {
			m.manualFP, cmd = m.manualFP.Update(msg)
		}
		return m, cmd
	}
	return m, nil
}

func (m *Model) onDiscovered(msg discoveredMsg) (*Model, tea.Cmd) {
	if msg.err != nil || msg.entry == nil {
		if msg.err != nil {
			m.setFlash(fmt.Sprintf("mDNS: %v", msg.err))
		}
		return m, m.enterManualEntry()
	}
	url := msg.entry.PairingURL()
	if url == "" {
		m.setFlash("mDNS entry incomplete; enter coordinator manually")
		return m, m.enterManualEntry()
	}
	m.coordURL = url
	m.coordName = msg.entry.Name
	// mDNS is URL-discovery only. The CA fingerprint never rides the
	// broadcast. Under --secure the operator pastes an OOB-verified
	// pin in the manual form; otherwise the pair runs in TOFU.
	if m.cfg.Secure {
		cmd := m.enterManualEntry()
		m.manualURL.SetValue(url)
		return m, tea.Batch(cmd, m.focusManualField(1))
	}
	m.state = stateConfirmCoordinator
	// Preflight the coord's policy so a strict coordinator pivots us
	// to the CA-entry form instead of letting the operator confirm a
	// pair that'll be rejected.
	m.policyInFlight = true
	return m, m.policyCmd(m.coordURL)
}

func (m *Model) onPairResp(msg pairRespMsg) (tea.Model, tea.Cmd) {
	// Drop stale responses from a prior generation — e.g. a poll in
	// flight when the operator pressed `r` to regenerate.
	if msg.gen != m.reqGen {
		return m, nil
	}
	if msg.err != nil {
		if errors.Is(msg.err, errAlreadyPairedWorker) {
			m.state = stateConfirmRepair
			return m, nil
		}
		m.err = msg.err
		m.state = stateFailed
		m.result = PairResult{Success: false, FailReason: msg.err.Error()}
		m.done = true
		return m, tea.Quit
	}
	switch msg.resp.Status {
	case "new", "existing":
		m.seenActive = true
		m.code = msg.resp.Code
		m.deadline = msg.resp.Deadline
		m.state = statePairing
		return m, tickCmd()
	case "cancelled":
		m.state = stateCancelled
		m.done = true
		return m, tea.Quit
	case "no_active_window":
		// Terminal error from the background loop takes precedence
		// over any success/mode heuristic — a trailing 403 means the
		// pair didn't actually complete.
		if msg.resp.Error != "" {
			m.err = errors.New(msg.resp.Error)
			m.state = stateFailed
			m.result = PairResult{Success: false, FailReason: msg.resp.Error}
			m.done = true
			return m, tea.Quit
		}
		// Mode=="worker" is the authoritative success signal: the
		// coord consumed the window and the node completed the
		// lifecycle flip. Anything else means the window closed
		// without success (expired, operator-cancelled, etc.).
		if msg.resp.Mode == "worker" {
			m.state = stateSuccess
			m.result = PairResult{
				Success:       true,
				Code:          m.code,
				CoordURL:      m.coordURL,
				CAFingerprint: m.caFingerprint,
			}
			m.done = true
			return m, tea.Quit
		}
		if m.seenActive {
			// Window existed but isn't complete: TTL likely elapsed.
			m.state = stateExpired
			return m, nil
		}
		m.err = errors.New("no active pairing window")
		m.state = stateFailed
		m.result = PairResult{Success: false, FailReason: "no active pairing window"}
		m.done = true
		return m, tea.Quit
	default:
		m.err = fmt.Errorf("unexpected pair status: %s", msg.resp.Status)
		m.state = stateFailed
		m.result = PairResult{Success: false, FailReason: m.err.Error()}
		m.done = true
		return m, tea.Quit
	}
}

func (m *Model) onTick() (tea.Model, tea.Cmd) {
	if m.state != statePairing {
		return m, nil
	}
	if m.cfg.Clock().After(m.deadline) {
		m.state = stateExpired
		return m, nil
	}
	// Poll the handler with empty body — reuse path returns the
	// active window's status without re-sending coord inputs. We
	// reuse the current generation so an in-flight poll doesn't get
	// dropped when it lands; only regenerate bumps the generation.
	return m, tea.Batch(m.pairCmd(m.reqGen, PairRequest{}), tickCmd())
}

func (m *Model) setFlash(s string) {
	m.flash = s
	m.flashUntil = m.cfg.Clock().Add(flashTTL)
}

// onCoordPolicy applies the preflight result. Drops responses that
// arrive after the wizard has already moved past confirm (e.g. the
// operator pressed `y` before preflight completed — the policyInFlight
// flag is supposed to prevent that, but the guard here is a second
// belt-and-braces). Fail-closed on transport error: without the
// response we can't tell whether the coord enforces strict mode, so
// continuing in TOFU would silently bypass strict coords under MITM.
func (m *Model) onCoordPolicy(msg coordPolicyMsg) (tea.Model, tea.Cmd) {
	m.policyInFlight = false
	// Late response arriving after we've moved on: drop it.
	if m.state != stateConfirmCoordinator && m.state != stateManualEntry {
		return m, nil
	}
	if msg.err != nil {
		friendly := FriendlyPreflightError(m.coordURL, msg.err)
		// Bounce back to the manual form with a one-line error so the
		// operator can correct the URL. Lazy-init the form if the
		// failure came from a flag-supplied / mDNS-confirmed URL.
		cmd := m.enterManualEntry()
		m.manualURL.SetValue(m.coordURL)
		m.manualErrMsg = friendly
		return m, cmd
	}
	// Preflight data (advertised fingerprint) came over an
	// unauthenticated TLS session, so we deliberately do NOT surface
	// it to the operator as an "OOB reference" — that would be a
	// contradiction. We only record the policy bit.
	if msg.requireSecure && m.caFingerprint == "" {
		// Coord rejects TOFU. Force Secure mode + drop back to the
		// manual form so the operator can paste an OOB-verified
		// fingerprint. URL is preserved; the fingerprint field gets
		// focus.
		m.cfg.Secure = true
		cmd := m.enterManualEntry()
		m.manualURL.SetValue(m.coordURL)
		return m, tea.Batch(cmd, m.focusManualField(1))
	}
	return m, nil
}

// onResetDone handles the POST /cluster/reset outcome. Success →
// retry the pair request with the stored coordinator inputs.
func (m *Model) onResetDone(msg resetDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.state = stateFailed
		m.result = PairResult{Success: false, FailReason: msg.err.Error()}
		m.done = true
		return m, tea.Quit
	}
	m.reqGen++
	m.state = stateRequesting
	return m, tea.Batch(m.pairCmd(m.reqGen, PairRequest{
		CoordinatorURL:           m.coordURL,
		CoordinatorCAFingerprint: m.caFingerprint,
	}), ui.SpinnerTickCmd())
}

// resetCmd POSTs to the local /cluster/reset admin endpoint.
func (m *Model) resetCmd() tea.Cmd {
	localPort := m.cfg.LocalPort
	adminKey := m.cfg.AdminAPIKey
	httpClient := m.cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		url := fmt.Sprintf("http://127.0.0.1:%d/zzrouter/v1/cluster/reset", localPort)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if adminKey != "" {
			req.Header.Set("X-API-Key", adminKey)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return resetDoneMsg{err: fmt.Errorf("local server unreachable: %w", err)}
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		if resp.StatusCode != http.StatusOK {
			return resetDoneMsg{err: errors.New(FriendlyPairHandlerError(resp.StatusCode, body))}
		}
		return resetDoneMsg{}
	}
}

// spinnerActive reports whether a spinner frame should be advanced
// for the current state. Keeping the animation scoped to long-
// running states keeps the TUI quiet (and CPU idle) otherwise.
func (m *Model) spinnerActive() bool {
	switch m.state {
	case stateDiscovering, stateRequesting:
		return true
	}
	return false
}
