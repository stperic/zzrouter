package views

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// Connect form — add a remote Ollama daemon as an ExternalProvider entry
// by POSTing to /zzrouter/v1/providers/instances. The form is a simple
// three-field wizard (name, endpoint, optional bearer token) that sits
// inside the existing providers view model under a dedicated view mode.
//
// The flow is reached from the Local provider picker: zzRouter inserts a
// synthetic `ollama-connect` entry alongside the shipped local providers
// so users don't have to hunt for a separate menu. Selecting it opens
// this form; submit fires the SDK call; result messages route back into
// the main ProvidersViewModel update loop.

// connectNameRe matches the server-side validation in providers_controller.go.
// Keeping it client-side lets us reject malformed names before round-tripping.
var connectNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

const (
	connectFieldName     = 0
	connectFieldEndpoint = 1
	connectFieldToken    = 2
	connectFieldCount    = 3
)

// providersConnectResultMsg carries the outcome of the add-provider-instance
// call. err is nil on success; name is echoed so the list refresh can
// surface a confirmation line.
type providersConnectResultMsg struct {
	name string
	err  error
}

// initConnectForm resets the form to the "ready for input" state. Called
// whenever the user enters the view from the picker — never reuses stale
// input from a prior abandoned attempt.
func (m *ProvidersViewModel) initConnectForm() {
	m.connectInputs = [connectFieldCount]textinput.Model{
		newConnectInput("ollama-nas", 40, false),
		newConnectInput("http://nas.lan:11434", 200, false),
		newConnectInput("optional bearer token", 512, true),
	}
	m.connectFocus = connectFieldName
	m.connectSubmitting = false
	m.connectErr = ""
}

func newConnectInput(placeholder string, limit int, secret bool) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = placeholder
	ti.CharLimit = limit
	if secret {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	s.Blurred.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ti.SetStyles(s)
	return ti
}

// focusConnectInput activates the ith input and blurs all others. Returns
// the focus command the textinput returns so the cursor blink animation
// starts immediately.
func (m *ProvidersViewModel) focusConnectInput(i int) tea.Cmd {
	m.connectFocus = i
	var cmd tea.Cmd
	for idx := range m.connectInputs {
		if idx == i {
			cmd = m.connectInputs[idx].Focus()
		} else {
			m.connectInputs[idx].Blur()
		}
	}
	return cmd
}

// updateConnectForm handles keys and textinput bubbling for the form view.
// Submit-in-flight locks all inputs until the result message arrives.
func (m *ProvidersViewModel) updateConnectForm(msg tea.Msg) tea.Cmd {
	// Forward every message to the currently focused input so cursor blink
	// and paste keep working during submit as well. We only block Enter
	// while in-flight; the rest of the UI should remain responsive.
	var tiCmd tea.Cmd
	m.connectInputs[m.connectFocus], tiCmd = m.connectInputs[m.connectFocus].Update(msg)

	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return tiCmd
	}

	// Match Esc explicitly rather than going through tui.ProvidersKeys.Back,
	// which is bound to ["esc", "q"] — using the binding here would kick
	// the user out of the form the moment they typed "q" in any field.
	isEsc := keyMsg.String() == "esc"

	if m.connectSubmitting {
		// Allow Esc to cancel while the SDK call is in flight. The late
		// result message is dropped in handleConnectResult by checking
		// that the form is still the active view.
		if isEsc {
			m.connectSubmitting = false
			m.connectErr = ""
			m.mode = providersViewPickProvider
			m.buildProviderPicker()
			return nil
		}
		return tiCmd
	}

	switch {
	case isEsc:
		m.mode = providersViewPickProvider
		m.connectErr = ""
		m.buildProviderPicker()
		return nil

	case keyMsg.String() == "tab":
		next := (m.connectFocus + 1) % connectFieldCount
		return m.focusConnectInput(next)

	case keyMsg.String() == "shift+tab":
		prev := (m.connectFocus - 1 + connectFieldCount) % connectFieldCount
		return m.focusConnectInput(prev)

	case key.Matches(keyMsg, tui.ProvidersKeys.Enter):
		// Advance focus from name/endpoint; submit from token (or from
		// any field once all required fields are filled).
		name := strings.TrimSpace(m.connectInputs[connectFieldName].Value())
		endpoint := strings.TrimSpace(m.connectInputs[connectFieldEndpoint].Value())
		if m.connectFocus == connectFieldName && name != "" && endpoint == "" {
			return m.focusConnectInput(connectFieldEndpoint)
		}
		if m.connectFocus == connectFieldEndpoint && endpoint != "" && name == "" {
			return m.focusConnectInput(connectFieldName)
		}
		// Validate before firing the request so users see mistakes inline.
		if !connectNameRe.MatchString(name) {
			m.connectErr = "name must match [a-z][a-z0-9-]{0,39}"
			return m.focusConnectInput(connectFieldName)
		}
		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			m.connectErr = "endpoint must include scheme + host (e.g. http://nas.lan:11434)"
			return m.focusConnectInput(connectFieldEndpoint)
		}

		m.connectSubmitting = true
		m.connectErr = ""
		token := m.connectInputs[connectFieldToken].Value()
		client := m.client
		return func() tea.Msg {
			_, err := client.AddProviderInstance(pkgClient.AddProviderInstanceRequest{
				Type:     "ollama-connect",
				Name:     name,
				Endpoint: endpoint,
				Token:    token,
			})
			return providersConnectResultMsg{name: name, err: err}
		}
	}

	return tiCmd
}

// handleConnectResult folds the SDK result back into the main view model.
// Success returns to the provider list with a refresh and a confirmation
// line; failure stays on the form with the error rendered inline so the
// user can correct and resubmit without retyping everything.
//
// Guarded against the Esc-mid-submit race: if the user cancelled and
// navigated elsewhere before the SDK call returned, the late result
// message must not clobber their current view. On error we route the
// focus to the name field specifically so 409 collisions drop the user
// exactly where they need to edit.
func (m *ProvidersViewModel) handleConnectResult(msg providersConnectResultMsg) tea.Cmd {
	if m.mode != providersViewConnectForm {
		// User Esc'd while the request was in flight. Drop the late result.
		return nil
	}
	m.connectSubmitting = false
	if msg.err != nil {
		m.connectErr = msg.err.Error()
		// Focus returns to name: 409 name-collision is the most common
		// retryable error and name is what the user needs to change.
		return m.focusConnectInput(connectFieldName)
	}
	m.actionStatus = fmt.Sprintf("✓ Added %s: scanning for models...", msg.name)
	m.mode = providersViewList
	m.loading = true
	m.needsRefresh = false
	client := m.client
	return tea.Batch(func() tea.Msg {
		apps, err := shared.FetchProviders(client, "", "", true)
		return providersLoadedMsg{providers: apps, err: err}
	}, shared.SpinnerTickCmd())
}

// viewConnectForm renders the three-field form. Layout mirrors the
// viewEnterKey style so the visual weight is consistent across the
// add-provider flows.
func (m *ProvidersViewModel) viewConnectForm(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString(" " + m.styles.Normal.Render("Add a remote Ollama daemon") + "\n")
	b.WriteString(" " + m.styles.Help.Render("zzRouter connects to an Ollama daemon you already run on your LAN or a remote host.") + "\n")
	b.WriteString("\n")

	labels := [connectFieldCount]string{
		"Name        ",
		"Endpoint    ",
		"Auth token  ",
	}
	hints := [connectFieldCount]string{
		"(lowercase, digits, hyphens: becomes the provider key)",
		"(full URL including scheme and port)",
		"(optional: only if your daemon requires bearer auth)",
	}

	for i := 0; i < connectFieldCount; i++ {
		marker := "  "
		if i == m.connectFocus {
			marker = "▶ "
		}
		line := fmt.Sprintf("%s%s%s", marker, m.styles.Normal.Render(labels[i]), m.connectInputs[i].View())
		b.WriteString(" " + line + "\n")
		b.WriteString("    " + m.styles.Help.Render(hints[i]) + "\n")
		b.WriteString("\n")
	}

	if m.connectErr != "" {
		b.WriteString(" " + m.styles.Error.Render("✗ "+m.connectErr) + "\n\n")
	}
	if m.connectSubmitting {
		b.WriteString(" " + shared.SpinnerFrame(m.loadTick) + " " + m.styles.Normal.Render("Adding provider…") + "\n\n")
	}

	footer := shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.ProvidersKeys.Enter, "submit / next field"),
		"tab/shift+tab cycle",
		tui.Hint(tui.ProvidersKeys.Back, "back"),
	))
	b.WriteString("\n" + footer)
	return b.String()
}
