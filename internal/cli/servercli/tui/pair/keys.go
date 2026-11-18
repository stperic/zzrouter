package pair

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stperic/common/atomicfile"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/ui"
)

// onKey dispatches key events by state. Key handling is kept here
// (not inline in Update) to keep the state machine readable. No
// method here mutates flash / in-flight state via goroutines —
// everything round-trips through Update via a tea.Msg so there is
// exactly one mutator.
func (m *Model) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Universal quit: Ctrl+C always exits. In statePairing, quit also
	// cancels the server-side window so we don't leak an open code.
	if key == "ctrl+c" {
		return m.quit()
	}

	switch m.state {
	case stateConfirmCoordinator:
		switch key {
		case "y", "Y", "enter":

			m.reqGen++
			m.state = stateRequesting
			return m, tea.Batch(m.pairCmd(m.reqGen, PairRequest{
				CoordinatorURL:           m.coordURL,
				CoordinatorCAFingerprint: m.caFingerprint,
			}), ui.SpinnerTickCmd())
		case "n", "N":
			m.state = stateManualEntry
			return m, nil
		case "q":
			return m.quit()
		}

	case stateManualEntry:
		return m.onManualEntryKey(msg, key)

	case stateConfirmRepair:
		switch key {
		case "y", "Y":
			return m, tea.Batch(m.resetCmd(), ui.SpinnerTickCmd())
		case "n", "N", "esc", "q":
			m.state = stateCancelled
			m.result = PairResult{Success: false, FailReason: "re-pair declined"}
			m.done = true
			return m, tea.Quit
		}

	case statePairing:
		switch key {
		case "c", "C":
			return m, m.copyCodeCmd()
		case "f", "F":
			return m, m.writeTransferFileCmd()
		case "r", "R":
			m.reqGen++
			m.state = stateRequesting
			m.seenActive = false
			return m, tea.Batch(m.pairCmd(m.reqGen, PairRequest{
				Regenerate:               true,
				CoordinatorURL:           m.coordURL,
				CoordinatorCAFingerprint: m.caFingerprint,
			}), ui.SpinnerTickCmd())
		case "q":
			return m.quit()
		}

	case stateExpired, stateSuccess, stateFailed, stateCancelled:
		if key == "q" || key == "enter" || key == "esc" {
			m.done = true
			return m, tea.Quit
		}
		if m.state == stateExpired && (key == "r" || key == "R") {
			m.reqGen++
			m.state = stateRequesting
			m.seenActive = false
			return m, tea.Batch(m.pairCmd(m.reqGen, PairRequest{
				Regenerate:               true,
				CoordinatorURL:           m.coordURL,
				CoordinatorCAFingerprint: m.caFingerprint,
			}), ui.SpinnerTickCmd())
		}
	}
	return m, nil
}

// onManualEntryKey handles keystrokes while the two-field manual
// entry form has focus. Navigation keys (tab/shift-tab/enter/esc)
// are intercepted; everything else is forwarded to the focused
// textinput so characters, backspace, arrow keys etc. work.
func (m *Model) onManualEntryKey(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.result = PairResult{Success: false, FailReason: "cancelled by operator"}
		m.done = true
		return m, tea.Quit
	case "tab", "down", "shift+tab", "up":
		// Only cycle focus when the fingerprint field is visible.
		if m.cfg.Secure {
			return m, m.focusManualField((m.manualFocus + 1) % 2)
		}
		return m, nil
	case "enter":
		return m.submitManualEntry()
	}
	// Forward to the focused textinput.
	var cmd tea.Cmd
	if m.manualFocus == 0 {
		m.manualURL, cmd = m.manualURL.Update(msg)
	} else {
		m.manualFP, cmd = m.manualFP.Update(msg)
	}
	return m, cmd
}

func (m *Model) focusManualField(idx int) tea.Cmd {
	m.manualFocus = idx
	if idx == 0 {
		m.manualFP.Blur()
		return m.manualURL.Focus()
	}
	m.manualURL.Blur()
	return m.manualFP.Focus()
}

func (m *Model) submitManualEntry() (tea.Model, tea.Cmd) {
	normalized, err := clusternode.NormalizeCoordinatorURL(m.manualURL.Value())
	if err != nil {
		m.manualErrMsg = err.Error()
		return m, m.focusManualField(0)
	}
	fp := strings.TrimSpace(m.manualFP.Value())
	if m.cfg.Secure && fp == "" {
		m.manualErrMsg = "CA fingerprint is required when --secure is set"
		return m, m.focusManualField(1)
	}
	// Reflect the normalized value back into the input so the
	// operator sees the scheme + port that'll actually be used.
	m.manualURL.SetValue(normalized)
	m.manualErrMsg = ""
	m.coordURL = normalized
	m.caFingerprint = fp
	m.coordName = "" // manual entry has no mDNS name
	m.manualURL.Blur()
	m.manualFP.Blur()
	m.state = stateConfirmCoordinator

	return m, nil
}

// quit is invoked on q/Ctrl+C. When an active pairing window is open,
// send a cancel to the server as a tea.Cmd and defer tea.Quit until
// the cancel response lands — so the window closes cleanly and the
// cancel HTTP call isn't orphaned by process exit.
func (m *Model) quit() (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	m.result = PairResult{Success: false, FailReason: "cancelled by operator"}
	if m.state == stateRequesting {
		m.quitting = true
		return m, nil
	}
	if m.state == statePairing {
		m.quitting = true
		m.state = stateCancelled
		return m, m.cancelCmd()
	}
	m.done = true
	return m, tea.Quit
}

// cancelCmd POSTs a cancel to the pair endpoint and returns
// cancelDoneMsg regardless of outcome. A bounded context caps the
// teardown latency; the cancelDoneMsg handler then returns tea.Quit.
func (m *Model) cancelCmd() tea.Cmd {
	post := m.cfg.PairPost
	if post == nil {
		post = m.defaultPairPost
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := post(ctx, PairRequest{Cancel: true})
		return cancelDoneMsg{err: err}
	}
}

// pairingDocument snapshots the observed window before a command performs I/O.
func (m *Model) pairingDocument() ([]byte, error) {
	transfer := clusternode.PairingWindowInfo{NodeName: m.cfg.NodeName, Code: m.code,
		WorkerURL: m.workerURL, Fingerprint: m.workerFingerprint, Deadline: m.deadline}.Transfer()
	if m.transfer != nil {
		transfer = *m.transfer
	}
	if err := transfer.Validate(m.cfg.Clock()); err != nil {
		return nil, err
	}
	return json.MarshalIndent(transfer, "", "  ")
}

// copyCodeCmd transfers the complete enrollment document through OSC52, which
// also works when the wizard runs over SSH on a headless computer.
func (m *Model) copyCodeCmd() tea.Cmd {
	data, err := m.pairingDocument()
	if err != nil {
		return func() tea.Msg { return flashMsg{text: "pairing document unavailable; prepare pairing again"} }
	}
	if m.cfg.Clipboard != nil {
		cp := m.cfg.Clipboard
		return func() tea.Msg {
			if err := cp(string(data)); err != nil {
				return flashMsg{text: "clipboard unavailable"}
			}
			return flashMsg{text: "pairing document copied"}
		}
	}
	return tea.Batch(tea.SetClipboard(string(data)), func() tea.Msg { return flashMsg{text: "pairing document copied"} })
}

// writeTransferFileCmd writes data, never an executable command, with owner-only access.
func (m *Model) writeTransferFileCmd() tea.Cmd {
	dir, fp := m.cfg.TransferFileDir, m.workerFingerprint
	data, err := m.pairingDocument()
	return func() tea.Msg {
		if err != nil {
			return flashMsg{text: "pairing document unavailable; prepare pairing again"}
		}
		if dir == "" {
			base, err := os.UserCacheDir()
			if err != nil {
				return flashMsg{text: "no writable state dir"}
			}
			dir = filepath.Join(base, "zzrouter")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return flashMsg{text: fmt.Sprintf("mkdir: %v", err)}
		}
		path := filepath.Join(dir, "pair-"+shortFP(fp)+".json")
		if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
			return flashMsg{text: fmt.Sprintf("write: %v", err)}
		}
		return flashMsg{text: "wrote " + path}
	}
}

// shortFP returns the first 8 hex chars of the fingerprint (after
// any sha256: prefix). Used as a filename suffix for the transfer
// document so multiple discovered coordinators don't collide.
func shortFP(fp string) string {
	if fp == "" {
		return "unknown"
	}
	hex := fp
	if len(fp) > 7 && fp[:7] == "sha256:" {
		hex = fp[7:]
	}
	if len(hex) < 8 {
		return hex
	}
	return hex[:8]
}

func shellWord(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// AcceptCommand formats the coordinator command with shell-safe enrollment inputs.
func AcceptCommand(code, workerURL, fingerprint string) string {
	return fmt.Sprintf("zzrouter cluster accept %s --worker-url %s --worker-fingerprint %s", shellWord(code), shellWord(workerURL), shellWord(fingerprint))
}
