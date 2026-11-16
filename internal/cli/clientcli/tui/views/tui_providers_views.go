package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"github.com/stperic/zzrouter/pkg/constants"
)

func (m *ProvidersViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading: m.loading && len(m.allProviders) == 0, Err: m.err, LoadTick: m.loadTick, BackHint: "R retry  Esc back",
	}); done {
		return out
	}
	if len(m.providers) == 0 {
		message := "No matching providers. X shows disabled."
		if len(m.allProviders) == 0 {
			message = "No providers. A connects a provider."
		}
		hints := "A connect  / search  X disabled  ? help  Esc back"
		if m.filterInput.Focused() {
			hints = "Enter apply  Esc clear"
		}
		return shared.TruncateText(message, width) + "\n" + shared.RenderViewFooterWithStatus(m.styles, width, m.providerListStatus(), hints)
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	// Pre-build all rows for stable column sizing
	allRowStrs := make([][]string, len(m.providers))
	for i, app := range m.providers {
		formats := shared.EmptyValue
		if len(app.Formats) > 0 {
			formats = strings.Join(app.Formats, ", ")
			if app.FormatNote != "" {
				formats = fmt.Sprintf("%s(%s)", formats, app.FormatNote)
			}
		}
		status := "Disabled"
		if app.Enabled {
			status = "Enabled"
		}
		allRowStrs[i] = []string{app.Name, app.Node, status, providerKind(app), versionCell(app), formats}
	}

	m.list.SetVisible(vis)
	start, end := m.list.VisibleRange()

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:        []string{"PROVIDER", "NODE", "STATUS", "KIND", "VERSION", "FORMATS"},
		ColumnPriority: []int{100, 80, 90, 70, 20, 10},
		ColWidths:      []int{18, 16, 10, 8, 14, 12},
		Width:          width,
		Vis:            vis,
		Cursor:         m.list.Cursor(),
		Offset:         m.list.Offset(),
		Styles:         m.styles,
		StatusCol:      -1,
		AllRows:        allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	status := m.providerListStatus()
	if line := m.actionLine(); line != "" {
		status = line
	}
	hints := "Enter details  / search  A connect  Tab kind  N node  X disabled  R refresh  ? help  Esc back"
	if m.filterInput.Focused() {
		status = m.providerListStatus()
		hints = "Enter apply  Esc clear"
	}
	if m.uninstallTarget != nil {
		status = fmt.Sprintf("Uninstall %s on %s?", m.uninstallTarget.Name, m.uninstallTarget.Node)
		hints = "Y uninstall  Enter/Esc cancel"
	}
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, hints))

	return b.String()
}

func (m *ProvidersViewModel) viewPicker(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder
	b.WriteString("\n")

	for i, item := range m.pickerItems {
		prefix := "  "
		if i == m.pickerCursor {
			prefix = "\u25b8 " // ▸
		}

		// Split "Label (hint)" so the hint renders in grey
		label, hint := item, ""
		if idx := strings.Index(item, " ("); idx >= 0 {
			label = item[:idx]
			hint = item[idx:]
		}

		if i == m.pickerCursor {
			b.WriteString(" " + m.styles.Selected.Render(prefix+label) + m.styles.Help.Render(hint) + "\n")
		} else {
			b.WriteString(" " + m.styles.Normal.Render(prefix+label) + m.styles.Help.Render(hint) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.ProvidersKeys.Up, "navigate"),
		tui.Hint(tui.ProvidersKeys.Enter, "select"),
		tui.Hint(tui.ProvidersKeys.Back, "back"),
	)))
	return b.String()
}

func (m *ProvidersViewModel) viewDetail(width, height int) string {
	d := shared.NewDetail(m.styles, width)

	app := m.selectedProvider()
	if app == nil {
		d.Write(" No app selected\n")
		return d.String()
	}

	d.Field("Name", app.Name)

	version := app.Version
	if version == "" {
		version = "unknown"
	}
	d.Field("Version", version)

	d.Field("Node", app.Node)

	mode := app.Mode
	if mode == "" {
		mode = constants.AppModeExternal
	}
	d.Field("Mode", mode)

	if len(app.Formats) > 0 {
		formats := strings.Join(app.Formats, ", ")
		if app.FormatNote != "" {
			formats += " (" + app.FormatNote + ")"
		}
		d.Field("Formats", formats)
	}

	if app.Enabled {
		d.Field("Status", "enabled")
	} else {
		d.Field("Status", "disabled")
	}

	// Upstream release reporting. Absent until the first check completes,
	// and it says why rather than leaving a gap that reads as "up to date".
	for _, line := range versionDetailLines(m.versionsReport) {
		d.Field(line[0], line[1])
	}

	// Provider rate limit status (from the providers/status API)
	if ps := m.providerStatus; ps != nil {
		d.Section("Rate Limits")
		if ps.CooldownSeconds > 0 {
			reason := cooldownReasonLabel(ps.CooldownReason)
			d.Field("Status", fmt.Sprintf("%s (%.0fs)", reason, ps.CooldownSeconds))
		} else {
			d.Field("Status", "ok")
		}
		if rl := ps.RateLimit; rl != nil {
			if rl.LimitRequests > 0 {
				d.Field("Requests", fmt.Sprintf("%d / %d remaining", rl.RemainingRequests, rl.LimitRequests))
			}
			if rl.LimitTokens > 0 {
				d.Field("Tokens", fmt.Sprintf("%d / %d remaining", rl.RemainingTokens, rl.LimitTokens))
			}
			if rl.ResetRequestsSeconds > 0 {
				d.Field("Resets In", fmt.Sprintf("%.0fs", rl.ResetRequestsSeconds))
			}
			d.Field("As Of", fmt.Sprintf("%.0fs ago", rl.ObservedAgoSeconds))
		} else {
			d.Write(" " + m.styles.Help.Render("No rate limit data yet") + "\n")
		}
	}

	d.Section("Parameters")

	if m.detailParamsLoading {
		d.Text(" ", "Loading parameters...")
	} else if m.detailParamsErr != nil {
		d.Text(" ", m.styles.Error.Render("Could not load parameters"))
	} else if len(m.detailParams) == 0 {
		d.Text(" ", "No parameters")
	} else {
		for _, parameter := range m.detailParams {
			value := parameter.Value
			if parameter.AutoResolvedHint != "" && parameter.AutoResolvedHint != value {
				value += " (" + parameter.AutoResolvedHint + ")"
			}
			d.Field(parameter.Key, value)
		}
	}
	footer := shared.RenderViewFooter(m.styles, width, tui.ProvidersKeys.DetailHintsString()+"  ? help")
	body := m.detail.Render(strings.TrimRight(d.String(), "\n"), max(height-shared.RenderedHeight(footer), 1))
	return body + "\n" + footer
}

// actionLine renders the status line under the table.
//
// A success is qualified while the table it describes is still being
// refetched: the notice and the version column would otherwise contradict
// each other on screen, which reads as the upgrade having done nothing.
func (m *ProvidersViewModel) actionLine() string {
	if m.actionStatus == "" || !m.actionUnconfirmed {
		return m.actionStatus
	}
	return m.actionStatus + ", refreshing…"
}
