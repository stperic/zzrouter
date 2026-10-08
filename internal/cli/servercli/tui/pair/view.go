package pair

import (
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/ui"
)

// render returns the wizard view for the current state. Colors are
// driven by the shared pkg/ui theme so the pair wizard matches the
// rest of the zzrouter TUIs.
func (m *Model) render() string {
	s := m.styles
	var b strings.Builder
	b.WriteString(s.Title.Render("zzrouter pairing") + "\n")
	b.WriteString(s.Help.Render(strings.Repeat("─", 40)) + "\n\n")

	switch m.state {
	case stateDiscovering:
		spin := s.Accent.Render(ui.SpinnerFrame(m.spinner))
		b.WriteString(spin + " Discovering coordinator on the LAN…\n")
	case stateConfirmCoordinator:
		renderConfirm(&b, m)
	case stateManualEntry:
		renderManualEntry(&b, m)
	case stateRequesting:
		spin := s.Accent.Render(ui.SpinnerFrame(m.spinner))
		b.WriteString(spin + " Opening pairing window…\n")
	case statePairing:
		renderPairing(&b, m)
	case stateSuccess:
		b.WriteString(s.Success.Render("Paired ✓") + "\n\n")
		b.WriteString(s.DetailKey.Render("Coordinator:  ") + m.coordURL + "\n")
		b.WriteString(s.DetailKey.Render("Fingerprint:  ") + m.caFingerprint + "\n\n")
		b.WriteString(s.HintsBar.Render("Press q or Enter to exit.") + "\n")
	case stateExpired:
		b.WriteString(s.Error.Render("Pairing window expired.") + "\n\n")
		b.WriteString(s.HintsBar.Render("[r] regenerate  [q] quit") + "\n")
	case stateFailed:
		b.WriteString(s.Error.Render("Pairing failed.") + "\n\n")
		if m.err != nil {
			b.WriteString(fmt.Sprintf("Error: %v\n\n", m.err))
		}
		b.WriteString(s.HintsBar.Render("Press q to quit.") + "\n")
	case stateCancelled:
		b.WriteString(s.HintsBar.Render("Pairing cancelled.") + "\n")
	case stateConfirmRepair:
		b.WriteString(s.Error.Render("This node is already paired as a worker.") + "\n\n")
		b.WriteString(s.HintsBar.Render("Re-pairing wipes the current cluster CA + signed cert.") + "\n")
		b.WriteString(s.HintsBar.Render("Existing mTLS connections to the current coordinator break.") + "\n\n")
		b.WriteString(s.HintsBar.Render("[y] wipe and re-pair  [n] quit") + "\n")
	}

	if m.flash != "" {
		b.WriteString("\n" + s.HintsBar.Render(m.flash) + "\n")
	}
	return b.String()
}

func renderManualEntry(b *strings.Builder, m *Model) {
	s := m.styles
	b.WriteString(s.HintsBar.Render("No coordinator discovered via mDNS: enter it manually.") + "\n\n")

	marker := func(active bool) string {
		if active {
			return s.Accent.Render("▸ ")
		}
		return "  "
	}
	b.WriteString(marker(m.manualFocus == 0) + s.DetailKey.Render("Coordinator URL") + "  " +
		s.HintsBar.Render("(e.g. https://coord.local:9091)") + "\n")
	b.WriteString("  " + m.manualURL.View() + "\n\n")

	if m.cfg.Secure {
		b.WriteString(marker(m.manualFocus == 1) + s.DetailKey.Render("CA fingerprint") + "  " +
			s.HintsBar.Render("(sha256:<hex> or bare hex)") + "\n")
		b.WriteString("  " + m.manualFP.View() + "\n\n")
		b.WriteString(s.HintsBar.Render("Obtain this out-of-band (ssh + 'zzrouter-node cluster ca-fingerprint') before pasting.") + "\n\n")
	} else {
		b.WriteString(s.HintsBar.Render("TLS pinning: OFF (TOFU). Pass --secure to require a CA fingerprint.") + "\n\n")
	}

	if m.manualErrMsg != "" {
		b.WriteString(s.Error.Render("⚠ "+m.manualErrMsg) + "\n\n")
	}
	hint := "[enter] submit  [esc] quit"
	if m.cfg.Secure {
		hint = "[tab] next field  [enter] submit  [esc] quit"
	}
	b.WriteString(s.HintsBar.Render(hint) + "\n")
}

func renderConfirm(b *strings.Builder, m *Model) {
	s := m.styles
	b.WriteString(s.Accent.Render("Found coordinator:") + "\n\n")
	if m.coordName != "" {
		b.WriteString(s.DetailKey.Render("  Name:        ") + m.coordName + "\n")
	}
	b.WriteString(s.DetailKey.Render("  URL:         ") + m.coordURL + "\n")
	if m.caFingerprint != "" {
		b.WriteString(s.DetailKey.Render("  Fingerprint: ") + m.caFingerprint + "\n")
		b.WriteString(s.DetailKey.Render("  Short form:  ") + s.Accent.Render(compactFingerprint(m.caFingerprint)) + "\n\n")
		b.WriteString(s.HintsBar.Render("Verify this matches the coordinator's CA before continuing.") + "\n\n")
	} else {
		b.WriteString(s.DetailKey.Render("  TLS pinning: ") + s.Error.Render("OFF (TOFU)") + "\n\n")
		b.WriteString(s.Error.Render("  On an untrusted network an active MITM can pin their own CA") + "\n")
		b.WriteString(s.Error.Render("  into this worker permanently. Only safe on trusted LANs.") + "\n")
		b.WriteString(s.HintsBar.Render("  Use --secure + --ca-fingerprint (verified out-of-band) for hostile networks.") + "\n\n")
	}
	b.WriteString(s.HintsBar.Render("[y] pair with this coordinator  [n] enter another  [q] quit") + "\n")
}

func renderPairing(b *strings.Builder, m *Model) {
	s := m.styles
	b.WriteString(s.DetailKey.Render("Coordinator: ") + m.coordURL + "\n")
	b.WriteString(s.DetailKey.Render("Fingerprint: ") + compactFingerprint(m.caFingerprint) + "\n\n")
	b.WriteString(s.HintsBar.Render("Pairing code:") + "\n\n")
	b.WriteString("  " + s.Accent.Render(formatCode(m.code)) + "\n\n")

	remaining := m.deadline.Sub(m.cfg.Clock()).Round(time.Second)
	if remaining < 0 {
		remaining = 0
	}
	b.WriteString(s.HintsBar.Render(fmt.Sprintf("Expires in %s.", remaining)) + "\n")
	b.WriteString(s.HintsBar.Render("Run 'zzrouter cluster accept <code>' on the coordinator.") + "\n\n")
	b.WriteString(s.HintsBar.Render("[c] copy code  [f] write transfer script  [r] regenerate  [q] cancel") + "\n")
}

// Also drop the local compactFingerprint in favor of a single helper
// shared with the rest of this file. Keeping it here (not unifying
// with the CLI's shortFingerprint in cluster_pair.go) because this
// package is intentionally CLI-free: the two will diverge if one
// side grows color codes, and that's OK — file-suffix shortFP (8
// chars) and operator-display compactFingerprint ("8…4") have
// different jobs.

// formatCode dash-groups a 16-char pairing code. Matches the
// cluster_pair.go plain-output formatting.
func formatCode(code string) string {
	if len(code) != 16 {
		return code
	}
	return code[0:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16]
}

// compactFingerprint returns "sha256:12345678…abcd" for operator display.
func compactFingerprint(fp string) string {
	if len(fp) < 20 {
		return fp
	}
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
