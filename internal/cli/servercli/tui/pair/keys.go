package pair

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

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
			if m.policyInFlight {
				// Don't let the operator race the preflight — a
				// late requireSecure=true would be a silent TOFU
				// downgrade if we opened a pair window first.
				m.setFlash("verifying coordinator policy…")
				return m, nil
			}
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
	normalized, err := NormalizeCoordinatorURL(m.manualURL.Value())
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
	// Preflight when the operator submitted a URL but no pin —
	// catches a strict-mode coord before the pair request opens.
	if fp == "" {
		m.policyInFlight = true
		return m, m.policyCmd(normalized)
	}
	return m, nil
}

// quit is invoked on q/Ctrl+C. When an active pairing window is open,
// send a cancel to the server as a tea.Cmd and defer tea.Quit until
// the cancel response lands — so the window closes cleanly and the
// cancel HTTP call isn't orphaned by process exit.
func (m *Model) quit() (tea.Model, tea.Cmd) {
	m.done = true
	if m.state == statePairing {
		m.state = stateCancelled
		m.result = PairResult{Success: false, FailReason: "cancelled by operator"}
		return m, m.cancelCmd()
	}
	m.result = PairResult{Success: false, FailReason: "cancelled by operator"}
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
		_, _ = post(ctx, PairRequest{Cancel: true})
		return cancelDoneMsg{}
	}
}

// copyCodeCmd copies the pairing code to the clipboard via OSC52. The
// escape sequence is written through bubbletea's program output, which
// the terminal emulator intercepts and routes to the local system
// clipboard — works over SSH into a headless worker where
// xclip/xsel/pbcopy aren't available. Supported by iTerm2, kitty,
// WezTerm, Alacritty, tmux (when set-clipboard is on), and VSCode's
// terminal. Test/injection callers override via cfg.Clipboard.
func (m *Model) copyCodeCmd() tea.Cmd {
	if m.cfg.Clipboard != nil {
		cp := m.cfg.Clipboard
		code := m.code
		return func() tea.Msg {
			if err := cp(code); err != nil {
				return flashMsg{text: "clipboard unavailable"}
			}
			return flashMsg{text: "code copied"}
		}
	}
	return tea.Batch(
		tea.SetClipboard(m.code),
		func() tea.Msg { return flashMsg{text: "code copied"} },
	)
}

// writeTransferFileCmd writes a one-liner acceptance script to
// $XDG_STATE_HOME/zzrouter/ (or os.UserCacheDir() fallback). Returns
// a flashMsg with the written path (or error text).
func (m *Model) writeTransferFileCmd() tea.Cmd {
	dir := m.cfg.TransferFileDir
	code := m.code
	fp := m.caFingerprint
	return func() tea.Msg {
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
		path := filepath.Join(dir, "pair-"+shortFP(fp)+".sh")
		script := fmt.Sprintf("#!/bin/sh\nzzrouter cluster accept %s\n", code)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			return flashMsg{text: fmt.Sprintf("write: %v", err)}
		}
		return flashMsg{text: "wrote " + path}
	}
}

// shortFP returns the first 8 hex chars of the fingerprint (after
// any sha256: prefix). Used as a filename suffix for the transfer
// script so multiple discovered coordinators don't collide.
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
