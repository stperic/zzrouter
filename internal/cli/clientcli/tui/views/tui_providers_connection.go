package views

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

type providerConnectionMsg struct {
	ctx      context.Context
	provider string
	status   *pkgClient.ProviderConnection
	test     *pkgClient.ConnectionTestResult
	applied  *pkgClient.ConnectionApplyResult
	err      error
}

func (m *ProvidersViewModel) cancelConnection() {
	if m.connectionCancel != nil {
		m.connectionCancel()
		m.connectionCancel = nil
	}
	m.connectionContext = nil
	m.connectionValues = nil
	m.keyInput.SetValue("")
	m.connectionBusy = false
}

func (m *ProvidersViewModel) beginConnection(provider string) tea.Cmd {
	m.cancelConnection()
	m.connection = nil
	m.connectionField = 0
	m.mode = providersViewEnterKey
	m.connectionBusy = true
	m.actionStatus = "Loading coordinator connection..."
	ctx, cancel := context.WithCancel(m.Context())
	m.connectionCancel = cancel
	m.connectionContext = ctx
	client := m.client
	return func() tea.Msg {
		status, err := client.GetProviderConnection(ctx, provider)
		return providerConnectionMsg{ctx: ctx, provider: provider, status: status, err: err}
	}
}

func (m *ProvidersViewModel) connectionInput() tea.Cmd {
	m.keyInput.Blur()
	if m.connection == nil || m.connectionField >= len(m.connection.Fields) {
		return nil
	}
	field := m.connection.Fields[m.connectionField]
	m.keyInput = newConnectInput(field.Label, 16<<10, field.Secret)
	m.keyInput.SetWidth(max(m.termWidth-4, 1))
	if !field.Secret {
		m.keyInput.SetValue(m.connectionValues[field.ID])
	}
	return m.keyInput.Focus()
}

func (m *ProvidersViewModel) handleConnectionResult(msg providerConnectionMsg) tea.Cmd {
	// The request context identifies this form's current operation across view lifetimes.
	if m.mode != providersViewEnterKey || msg.ctx == nil || msg.ctx != m.connectionContext || msg.ctx.Err() != nil || msg.provider != m.installProvider {
		return nil
	}
	m.connectionBusy = false
	if msg.err != nil {
		m.actionStatus = "Connection request failed: " + msg.err.Error()
		return nil
	}
	if msg.status != nil {
		m.connection = msg.status
		m.connectionValues = maps.Clone(msg.status.Values)
		if m.connectionValues == nil {
			m.connectionValues = map[string]string{}
		}
		m.actionStatus = "Enter a complete replacement. Existing secrets are never shown."
		return m.connectionInput()
	}
	if msg.test != nil {
		m.actionStatus = "Test: " + msg.test.Code + ". No changes were saved."
		return nil
	}
	if msg.applied != nil {
		m.connection = msg.applied.Connection
		m.connectionValues = map[string]string{}
		m.keyInput.SetValue("")
		m.needsRefresh = true
		m.connectionField = 0
		m.actionStatus = fmt.Sprintf("%s. Active: %t. Published: %t. Durable: %t.", msg.applied.Outcome, msg.applied.Active, msg.applied.Published, msg.applied.Durable)
		if m.connection != nil {
			m.connectionValues = maps.Clone(m.connection.Values)
			return m.connectionInput()
		}
	}
	return nil
}

func (m *ProvidersViewModel) updateEnterKey(msg tea.Msg) tea.Cmd {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		if keyMsg.String() == "esc" {
			m.cancelConnection()
			m.mode = providersViewPickProvider
			m.actionStatus = ""
			m.buildProviderPicker()
			return nil
		}
		if m.connectionBusy {
			return nil
		}
		if keyMsg.String() == "ctrl+r" {
			return m.beginConnection(m.installProvider)
		}
		if m.connection == nil {
			return nil
		}
		switch keyMsg.String() {
		case "ctrl+d":
			return m.connectionCommand("remove")
		case "ctrl+e":
			return m.connectionCommand("external")
		case "ctrl+t":
			return m.connectionCommand("test")
		case "ctrl+s":
			return m.connectionCommand("save")
		case "enter":
			if m.connectionField < len(m.connection.Fields) {
				field := m.connection.Fields[m.connectionField]
				value := m.keyInput.Value()
				if !field.Secret {
					value = strings.TrimSpace(value)
				}
				if err := field.ValidateValue(value, true); err != nil {
					m.actionStatus = err.Error()
					return nil
				}
				m.connectionValues[field.ID] = value
				m.connectionField++
				return m.connectionInput()
			}
		case "shift+tab":
			if m.connectionField > 0 {
				m.connectionField--
				return m.connectionInput()
			}
		}
	}
	if m.connectionBusy || m.connection == nil || m.connectionField >= len(m.connection.Fields) {
		return nil
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return cmd
}

func (m *ProvidersViewModel) connectionCommand(action string) tea.Cmd {
	if action != "test" && !m.connection.ManagedAvailable {
		m.actionStatus = "Managed changes require security.credentials.key_file on the coordinator."
		return nil
	}
	if action == "save" || action == "test" {
		if m.connectionField < len(m.connection.Fields) {
			field := m.connection.Fields[m.connectionField]
			m.connectionValues[field.ID] = m.keyInput.Value()
		}
		for _, field := range m.connection.Fields {
			if err := field.ValidateValue(m.connectionValues[field.ID], true); err != nil {
				m.actionStatus = err.Error()
				return nil
			}
		}
	}
	m.connectionBusy = true
	m.actionStatus = "Applying connection request..."
	ctx := m.Context()
	if m.connectionCancel != nil {
		m.connectionCancel()
	}
	ctx, m.connectionCancel = context.WithCancel(ctx)
	m.connectionContext = ctx
	client, provider, revision := m.client, m.installProvider, m.connection.Revision
	values := maps.Clone(m.connectionValues)
	return func() tea.Msg {
		msg := providerConnectionMsg{ctx: ctx, provider: provider}
		switch action {
		case "test":
			msg.test, msg.err = client.TestProviderConnection(ctx, provider, values)
		case "save":
			msg.applied, msg.err = client.SaveProviderConnection(ctx, provider, revision, values, true)
		case "remove":
			msg.applied, msg.err = client.RemoveProviderConnection(ctx, provider, revision)
		case "external":
			msg.applied, msg.err = client.UseExternalProviderConnection(ctx, provider, revision)
		}
		return msg
	}
}

func (m *ProvidersViewModel) viewEnterKey(width, height int) string {
	contentWidth := max(width-2, 1)
	line := func(value string) string { return " " + shared.TruncateText(value, contentWidth) }
	lines := []string{line("Connection: " + m.installProvider)}
	if m.connection != nil {
		present := false
		for _, field := range m.connection.Fields {
			if field.Secret && slices.Contains(m.connection.Present, field.ID) {
				present = true
			}
		}
		presence := "absent"
		if present {
			presence = "present"
		}
		if height >= 10 {
			lines = append(lines, line(fmt.Sprintf("Secret: %s | %s r%d", presence, m.connection.Mode, m.connection.Revision)))
		}
		if height >= 16 {
			lines = append(lines, line("Coordinator: "+m.client.BaseURL()))
		}
		if m.connectionField < len(m.connection.Fields) {
			field := m.connection.Fields[m.connectionField]
			label := field.Label
			if !field.Required {
				label += " (optional)"
			}
			lines = append(lines, line(fmt.Sprintf("%s (%d/%d)", label, m.connectionField+1, len(m.connection.Fields))), " "+m.keyInput.View())
		} else {
			lines = append(lines, line("Candidate ready. Test or save and activate."))
		}
	}
	footer := []string{"Enter next | Shift+Tab previous", "Ctrl+T test | Ctrl+S save/activate", "Ctrl+D remove | Ctrl+E external", "Ctrl+R reload | Esc cancel"}
	if width < 36 || height < 10 {
		footer = []string{"^T test | ^S save/activate", "^D remove | ^E external", "^R reload | Esc cancel"}
		if height >= 10 {
			footer = append([]string{"Enter next | Shift+Tab back"}, footer...)
		}
	}
	status := m.actionStatus
	if m.connection != nil && !m.connection.ManagedAvailable {
		status = "Protected writes unavailable: configure security.credentials.key_file on the coordinator. " + status
	}
	if status != "" {
		statusLines := strings.Split(shared.WrapText(status, contentWidth), "\n")
		budget := max(1, height-len(lines)-len(footer)-1)
		if len(statusLines) > budget {
			statusLines = statusLines[:budget]
			statusLines[len(statusLines)-1] = shared.TruncateText(statusLines[len(statusLines)-1]+"...", contentWidth)
		}
		for _, value := range statusLines {
			lines = append(lines, " "+m.styles.Help.Render(value))
		}
	}
	lines = append(lines, "")
	for _, value := range footer {
		lines = append(lines, line(value))
	}
	return strings.Join(lines, "\n")
}
