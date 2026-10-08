package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"github.com/stperic/zzrouter/pkg/constants"
)

func (m *ProvidersViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	// Providers folds a connection error into the empty .State: if the API
	// call fails we still want the "install a provider" invitation rather
	// than a raw error dump. So err → empty (not Err:), and EmptyText
	// carries the multi-line help.
	emptyMsg := "No inference providers installed.\n\n" +
		m.styles.Help.Render(" Press a to install a local provider (Ollama, vLLM, MLX, llama.cpp)") + "\n" +
		m.styles.Help.Render(" or a cloud provider (OpenAI, Anthropic, Azure, Gemini).")
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Empty:     m.err != nil || len(m.providers) == 0,
		EmptyText: emptyMsg,
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
		EmptyHint: tui.JoinHints(tui.Hint(tui.ProvidersKeys.Install, "install"), tui.Hint(tui.ListKeys.Back, "back")),
	}); done {
		return out
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	// Pre-build all rows for stable column sizing
	allRowStrs := make([][]string, len(m.providers))
	for i, app := range m.providers {
		mode := app.Mode
		if mode == "" {
			mode = constants.AppModeExternal
		}
		formats := shared.EmptyValue
		if len(app.Formats) > 0 {
			formats = strings.Join(app.Formats, ", ")
			if app.FormatNote != "" {
				formats = fmt.Sprintf("%s(%s)", formats, app.FormatNote)
			}
		}
		allRowStrs[i] = []string{app.Name, versionCell(app), app.Node, mode, formats}
	}

	start, end := m.list.VisibleRange()

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"PROVIDER", "VERSION", "NODE", "MODE", "FORMATS"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	if line := m.actionLine(); line != "" {
		switch {
		case strings.HasPrefix(line, "✓"):
			b.WriteString(" " + m.styles.Success.Render(line) + "\n")
		case strings.HasPrefix(line, "✗"):
			b.WriteString(" " + m.styles.Error.Render(line) + "\n")
		default:
			b.WriteString(" " + m.styles.Help.Render(line) + "\n")
		}
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, shared.TableStatus("Providers", len(m.providers), m.list.Offset(), vis), tui.ProvidersKeys.ListHintsString()))

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

	// --- Fixed footer ---
	footer := "\n" + shared.RenderViewFooter(m.styles, width, tui.ProvidersKeys.DetailHintsString())

	// Measure header and footer dynamically to compute available rows
	headerH := shared.RenderedHeight(d.String())
	footerH := shared.RenderedHeight(footer)
	availRows := max(height-headerH-footerH, 1)

	// --- Scrollable middle: parameters ---
	if m.detailParamsLoading {
		d.Write(" Loading...\n")
	} else if m.detailParamsErr != nil {
		d.Write(" " + m.styles.Help.Render("Could not load parameters") + "\n")
	} else if len(m.detailParams) == 0 {
		d.Write(" " + m.styles.Help.Render("No parameters") + "\n")
	} else {
		keyW := max(width/3, 20)
		if keyW > 44 {
			keyW = 44
		}
		maxValW := max(width-keyW-4, 10)

		// Clamp scroll
		maxScroll := max(len(m.detailParams)-availRows, 0)
		scroll := min(m.detail.Scroll(), maxScroll)

		end := min(scroll+availRows, len(m.detailParams))

		for i := scroll; i < end; i++ {
			p := m.detailParams[i]
			keyStr := shared.TruncateText(p.Key, keyW-2)

			valStr := p.Value
			if p.AutoResolvedHint != "" && p.AutoResolvedHint != p.Value {
				valStr += " (" + p.AutoResolvedHint + ")"
			}
			valStr = shared.TruncateText(valStr, maxValW)

			line := fmt.Sprintf(" %-*s %s", keyW, keyStr, valStr)
			d.Write(m.styles.Normal.Render(line) + "\n")
		}
	}

	d.Write(footer)

	return d.String()
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
