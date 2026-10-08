package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/lipgloss/v2"
)

// withRefreshHint appends a spinner to the footer hints while a background
// reload is in flight. Appended rather than given its own status line so the
// body does not shift by two rows every time the view reloads — which means
// it must fit on the hints line, or the wrap reintroduces the shift it was
// avoiding. Hints win; the spinner is the droppable part.
func (m *ModelsViewModel) withRefreshHint(hints string, width int) string {
	if !m.fetching {
		return hints
	}
	suffix := "   " + shared.SpinnerFrame(m.loadTick) + " refreshing"
	if lipgloss.Width(hints)+lipgloss.Width(suffix) > width-1 {
		return hints
	}
	return hints + suffix
}

func (m *ModelsViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.rows) == 0,
		EmptyText: "No models found",
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
	}); done {
		return out
	}

	var b strings.Builder

	// Confirmation dialog overlay
	if m.confirmKind != confirmNone {
		b.WriteString(" " + m.styles.Error.Render(m.confirmMessage) + "\n\n")
	}

	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
	if m.confirmKind != confirmNone {
		vis -= shared.TuiConfirmLines
	}
	if vis < 3 {
		vis = 3
	}

	// Pre-build all rows as strings for stable column sizing across pages
	allRowStrs := make([][]string, len(m.rows))
	for i, r := range m.rows {
		allRowStrs[i] = []string{r.Model, r.Provider, r.Node, r.Size, r.Status}
	}

	start, end := shared.TableSliceBounds(m.list.Offset(), vis, len(m.rows))
	visibleRows := m.rows[start:end]

	rowStyles := make([]lipgloss.Style, len(visibleRows))
	for i, r := range visibleRows {
		rowStyles[i] = shared.StatusStyle(m.styles, r.Status)
	}

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"MODEL", "PROVIDER", "NODE", "SIZE", "STATUS"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: 4,
		RowStyles: rowStyles,
		AllRows:   allRowStrs,
	})

	for _, r := range visibleRows {
		t.Row(r.Model, r.Provider, r.Node, r.Size, r.Status)
	}

	b.WriteString(t.Render() + "\n")

	sortLabel := m.sortBy
	if sortLabel == "" {
		sortLabel = "status"
	}
	status := shared.TableStatus("Models", len(m.rows), m.list.Offset(), vis) + " | Sort: " + sortLabel
	if m.statusMsg != "" {
		status += "  " + m.statusMsg
	}
	var selectedRow *modelRow
	c := m.list.Cursor()
	if c >= 0 && c < len(m.rows) {
		selectedRow = &m.rows[c]
	}
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, m.withRefreshHint(modelsListHints(selectedRow), width)))

	return b.String()
}

func (m *ModelsViewModel) viewDetail(width, height int) string {
	r := m.selectedRow()
	if r == nil {
		return " No model selected\n"
	}

	d := shared.NewDetail(m.styles, width)

	d.Field("Model", r.RawModel)
	d.Field("Node", r.Node)
	d.Field("Provider", r.Provider)
	if r.registry != nil && r.registry.Size > 0 {
		d.Field("Size", shared.FormatSize(r.registry.Size))
	} else {
		d.Field("Size", r.Size)
	}
	d.Field("Status", r.Status)

	if r.registry != nil {
		reg := r.registry
		if reg.GetFormat() != "" || reg.GetQuantization() != "" || reg.SourceRepo != "" {
			d.Section("Metadata")
			if f := reg.GetFormat(); f != "" {
				d.Field("Format", f)
			}
			if q := reg.GetQuantization(); q != "" {
				d.Field("Quantization", q)
			}
			if repo := reg.SourceRepo; repo != "" {
				d.Field("Source", repo)
			}
			if t := reg.GetModifiedTime(); !t.IsZero() {
				d.Field("Modified", t.Format("2006-01-02 15:04"))
			}
		}
	}

	// Usage since node start. Attribution-free, so it is populated for a
	// locally deployed model that no key ever authenticated against.
	lines := usageDetailLines(m.modelUsage)
	if len(lines) == 0 && m.modelUsageIdle {
		lines = idleUsageLines()
	}
	if len(lines) > 0 {
		d.Section("Usage since restart")
		for _, line := range lines {
			d.Field(line[0], line[1])
		}
	}

	if r.instance != nil {
		inst := r.instance
		d.Section("Runtime")
		d.Field("Instance ID", inst.ID)
		if inst.SizeBytes > 0 {
			d.Field("Memory", shared.FormatSize(inst.SizeBytes))
		}
		if inst.Port > 0 {
			d.Field("Port", fmt.Sprintf("%d", inst.Port))
		}
		if inst.StartedAt != "" && inst.StartedAt != "0001-01-01T00:00:00Z" {
			d.Field("Started", inst.StartedAt)
		}
		if until := formatCompactUntil(inst.KeepAlive, inst.LastActivity, inst.StartedAt); until != "" {
			d.Field("Expires", until+" left")
		}
		if inst.Processor != "" {
			d.Field("Processor", inst.Processor)
		}
		if inst.ContextLength > 0 {
			d.Field("Context", fmt.Sprintf("%d", inst.ContextLength))
		}
	}

	m.appendPingSection(d, r)

	if r.deploy != nil {
		p := r.deploy
		d.Section("Download")
		d.Field("Progress", p.Progress)
		d.Field("Speed", p.Speed)
		d.Field("ETA", p.ETA)
		if p.Error != "" {
			d.Field("Error", p.Error)
		}
	}

	// Split into lines and scroll
	allLines := strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
	footer := shared.RenderViewFooter(m.styles, width, m.withRefreshHint(modelsDetailHints(m.selectedRow()), width))
	extraLines := 0
	if m.confirmKind != confirmNone {
		extraLines = shared.TuiConfirmLines
	}
	result, _ := shared.RenderDetailView(m.styles, allLines, m.detail.Scroll(), height, footer, "", extraLines)

	// Insert confirmation dialog after the header if pending
	if m.confirmKind != confirmNone {
		confirmLine := " " + m.styles.Error.Render(m.confirmMessage) + "\n\n"
		// Insert after the header (first 2 lines: title + blank)
		headerEnd := strings.Index(result, "\n\n")
		if headerEnd >= 0 {
			result = result[:headerEnd+2] + confirmLine + result[headerEnd+2:]
		}
	}
	return result
}

// modelsListHints returns the footer hints for the models list view.
func modelsListHints(row *modelRow) string {
	hints := []string{
		tui.Hint(tui.ModelsKeys.Up, "navigate"),
		tui.Hint(tui.ModelsKeys.Enter, "details"),
	}
	if row != nil && row.isRoute() {
		hints = append(hints, tui.Hint(tui.ModelsKeys.Space, "expand"))
	}
	hints = append(hints,
		tui.Hint(tui.ModelsKeys.New, "new route"),
		tui.Hint(tui.ModelsKeys.Sort, "sort"),
		tui.Hint(tui.ModelsKeys.Refresh, "refresh"),
		tui.Hint(tui.ModelsKeys.Back, "back"),
	)
	return tui.JoinHints(hints...)
}

// modelsDetailHints returns the footer hints for the models detail view.
func modelsDetailHints(row *modelRow) string {
	hints := []string{
		tui.Hint(tui.ModelsKeys.Edit, "parameters"),
	}
	isDownloading := row != nil && row.deploy != nil
	if !isDownloading {
		hints = append(hints,
			tui.Hint(tui.ModelsKeys.Test, "test"),
			tui.Hint(tui.ModelsKeys.Chat, "chat"),
		)
		if row != nil && !row.IsCloud {
			hints = append(hints, tui.Hint(tui.ModelsKeys.StartStop, "start/stop"))
		}
	}
	hints = append(hints,
		tui.Hint(tui.ModelsKeys.Delete, "delete"),
		tui.Hint(tui.ModelsKeys.Back, "back"),
	)
	return tui.JoinHints(hints...)
}
