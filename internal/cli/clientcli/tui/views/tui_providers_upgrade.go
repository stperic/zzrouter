package views

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
)

// Upgrade prompt — free-text version override.
//
// Blank input installs the newest release upstream publishes. It is
// deliberately not the config pin: the pin seeds a *fresh* install, so
// resolving an upgrade to it is a no-op whenever the node already matches
// and a downgrade after any earlier one-off upgrade. Typing a version
// overrides both for this run; ctrl+p additionally persists it as the pin so
// later installs agree. The two are separate because a one-off "try this
// build" and "this cluster standardizes on this build" are different
// intents, and only the second should rewrite cluster config.

// versionInputCharLimit is generous enough for any upstream release tag
// (llama.cpp "b10453", ollama "0.21.2", vLLM "0.19.1").
const versionInputCharLimit = 64

// versionInputWidth is wide enough to render the placeholder unclipped;
// textinput defaults to a narrow box that truncates it to one character.
const versionInputWidth = 44

// prefillUpgradeInput seeds the prompt with the newest upstream release when
// one is known to be newer.
//
// Filling it in rather than leaving it blank shows the operator exactly what
// enter will install. The two resolve to the same release, but only one of
// them says so before the job starts.
func (m *ProvidersViewModel) prefillUpgradeInput(app *shared.ProviderInfo) {
	if app == nil {
		return
	}
	if target := upgradeTargetFor(*app); target != "" {
		m.upgradeInput.SetValue(target)
	}
}

// latestForPrompt describes where the provider stands against upstream.
//
// An unknown result is reported as unknown rather than as "up to date": a
// check that never completed and a check that came back equal are different
// facts, and only one of them means there is nothing to do.
//
// No em dashes: this is UI copy.
func latestForPrompt(latest, status string) string {
	switch {
	case latest == "":
		return "unknown, upstream has not been checked"
	case status == versionStatusNewer:
		return latest + " (update available)"
	case status == versionStatusSame:
		return latest + " (already up to date)"
	default:
		return latest
	}
}

// newVersionInput creates the free-text input for an upgrade target version.
func newVersionInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = versionInputCharLimit
	ti.SetWidth(versionInputWidth)
	ti.Placeholder = "leave blank for the newest upstream release"
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	s.Blurred.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ti.SetStyles(s)
	return ti
}

// upgradePinKey toggles "also persist as the config pin" on the prompt.
var upgradePinKey = key.NewBinding(key.WithKeys("ctrl+p"), key.WithHelp("ctrl+p", "toggle pin"))

// updateUpgrade handles keys for the upgrade version prompt.
func (m *ProvidersViewModel) updateUpgrade(msg tea.Msg) tea.Cmd {
	var tiCmd tea.Cmd
	m.upgradeInput, tiCmd = m.upgradeInput.Update(msg)

	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return tiCmd
	}

	switch {
	case key.Matches(keyMsg, tui.ProvidersKeys.Back):
		m.mode = providersViewList
		m.upgradeInput.Blur()
		m.actionStatus = ""
		return nil

	case key.Matches(keyMsg, upgradePinKey):
		m.upgradePin = !m.upgradePin
		return tiCmd

	case key.Matches(keyMsg, tui.ProvidersKeys.Enter):
		version := strings.TrimSpace(m.upgradeInput.Value())
		// Pinning needs something to pin; a blank pin request would clear
		// the pin rather than set one, which is not what ctrl+p advertises.
		if m.upgradePin && version == "" {
			m.actionStatus = "✗ Type a version to pin, or press ctrl+p to upgrade without pinning"
			return tiCmd
		}
		m.mode = providersViewList
		m.upgradeInput.Blur()
		// Deliberately NOT m.loading: the list prelude short-circuits to a
		// bare spinner while loading, which hides the very status line the
		// upgrade streams its progress into. The list stays on screen and
		// the progress renders beneath it; the terminal handler sets loading
		// for the brief refetch once the job is done.

		client := m.client
		name, node := m.upgradeProvider, m.upgradeNode
		pin := m.upgradePin
		// A holding line until the first job frame replaces it with the
		// real step. Blank means "resolve the newest upstream release",
		// which is what upgrade does when no version is given.
		if version == "" {
			m.actionStatus = fmt.Sprintf("Upgrading %s on %s to the latest release…", name, node)
		} else {
			m.actionStatus = fmt.Sprintf("Upgrading %s on %s to %s…", name, node, version)
		}

		// Pin first when asked: the pin governs what a fresh install of
		// this provider starts on, so persisting it before the upgrade
		// keeps the two consistent even if the upgrade later fails.
		if pin {
			if err := client.SetProviderPinnedVersion(name, version, ""); err != nil {
				return func() tea.Msg {
					return providersActionMsg{action: "upgrade", name: name, err: err}
				}
			}
		}
		// Stream the job rather than reporting on the 202: an upgrade is
		// asynchronous, and calling it succeeded the moment the request is
		// accepted claims an outcome nobody has observed yet.
		return tea.Batch(
			startUpgradeJobCmd(m.Context(), client, name, version, node),
			shared.SpinnerTickCmd(),
		)
	}

	return tiCmd
}

// viewUpgrade renders the version prompt.
func (m *ProvidersViewModel) viewUpgrade(width, _ int) string {
	var b strings.Builder

	// Title carries its own Padding(0, 1); adding a leading space here
	// would push it one column right of every other row.
	b.WriteString("\n")
	b.WriteString(m.styles.Title.Render(fmt.Sprintf("Upgrade %s", m.upgradeProvider)))
	b.WriteString("\n\n")

	d := shared.NewDetail(m.styles, width)
	d.Field("Node", m.upgradeNode)
	if m.upgradeCurrent != "" {
		d.Field("Installed", m.upgradeCurrent)
	}
	// Without this the prompt for an up-to-date provider is indistinguishable
	// from one with an update waiting: same empty field, same keys. Pressing
	// enter then reinstalls the version already there and reads as nothing
	// having happened.
	d.Field("Latest", latestForPrompt(m.upgradeLatest, m.upgradeStatus))
	b.WriteString(d.String())
	b.WriteString("\n")

	b.WriteString(" Target version\n ")
	b.WriteString(m.upgradeInput.View())
	b.WriteString("\n\n")

	pinMark := "off"
	if m.upgradePin {
		pinMark = "on"
	}
	b.WriteString(fmt.Sprintf(" Save as config pin (ctrl+p): %s\n", pinMark))
	if m.upgradePin {
		b.WriteString(m.styles.Help.Render(" The pin is written on the coordinator and synced to every node.\n"))
	} else {
		b.WriteString(m.styles.Help.Render(" This run only; the configured pin is left unchanged.\n"))
	}

	b.WriteString("\n")
	b.WriteString(m.styles.Help.Render(tui.JoinHints(
		tui.Hint(tui.ProvidersKeys.Enter, "upgrade"),
		tui.Hint(upgradePinKey, "toggle pin"),
		tui.Hint(tui.ProvidersKeys.Back, "cancel"),
	)))
	b.WriteString("\n")

	return b.String()
}
