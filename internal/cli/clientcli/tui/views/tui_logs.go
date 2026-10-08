package views

import (
	"context"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

type logsLoadedMsg struct {
	entries []pkgClient.InferenceLogEntry
	err     error
}

type logsNewEntryMsg struct {
	entry pkgClient.InferenceLogEntry
}

type logsStreamDoneMsg struct {
	err error
}

// Content caps for the log detail pane: now that long values wrap instead of
// running off the edge, an uncapped prompt or message would push everything
// below it out of reach behind many screens of scrolling.
const (
	maxDetailPromptRunes  = 500
	maxDetailMessageRunes = 300
	// The reply is the reason to open an entry, so it gets more room
	// than the request messages that precede it.
	maxDetailResponseRunes = 2000
)

type logsViewMode int

const (
	logsViewList logsViewMode = iota
	logsViewDetail
	logsViewPayload
)

// logItem wraps an inference log entry to satisfy tui.Item.
type logItem struct{ pkgClient.InferenceLogEntry }

func (l logItem) Title() string       { return l.Model }
func (l logItem) Description() string { return l.ID }
func (l logItem) FilterValue() string { return l.Model + " " + l.ID }

type LogsViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	entries    []pkgClient.InferenceLogEntry
	list       tui.List
	detail     tui.Detail
	termWidth  int
	termHeight int
	loading    bool
	err        error
	mode       logsViewMode
	loadTick   int

	// Payload pane (retained request bodies for one entry). It owns a
	// separate scroll so opening it does not reset the detail pane's.
	// openFile is a field so tests never launch a real application.
	openFile       func(string) error
	payload        *pkgClient.InferenceLogPayload
	payloadDetail  tui.Detail
	payloadLines   []string
	payloadID      string
	payloadModel   string
	payloadReturn  logsViewMode
	payloadLoading bool
	payloadErr     error
	payloadStatus  string

	// Follow mode (SSE streaming)
	following    bool
	cancelFollow context.CancelFunc
	streamCh     chan tea.Msg
}

func NewLogsViewModel(client *pkgClient.Client, styles ui.Styles) *LogsViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up:       tui.ListKeys.Up,
		Down:     tui.ListKeys.Down,
		PageUp:   tui.ListKeys.PageUp,
		PageDown: tui.ListKeys.PageDown,
		Home:     tui.ListKeys.Home,
		End:      tui.ListKeys.End,
	})
	detailKeys := tui.NavigationKeys{
		Up:       tui.ListKeys.Up,
		Down:     tui.ListKeys.Down,
		PageUp:   tui.ListKeys.PageUp,
		PageDown: tui.ListKeys.PageDown,
		Home:     tui.ListKeys.Home,
		End:      tui.ListKeys.End,
	}
	detail := tui.NewDetail()
	detail.SetKeys(detailKeys)
	payloadDetail := tui.NewDetail()
	payloadDetail.SetKeys(detailKeys)
	return &LogsViewModel{client: client, styles: styles, loading: true,
		list: list, detail: detail, payloadDetail: payloadDetail,
		openFile: shared.OpenFile}
}

func (m *LogsViewModel) syncLogItems() {
	items := make([]tui.Item, len(m.entries))
	for i, e := range m.entries {
		items[i] = logItem{e}
	}
	m.list.SetItems(items)
}

// Init implements tea.Model.
func (m *LogsViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchLogsCmd(), shared.SpinnerTickCmd())
}

// Refresh implements tui.Refresher: a child view (chat) generates inference
// entries, so reload the tail when it pops.
func (m *LogsViewModel) Refresh() tea.Cmd { return m.fetchLogsCmd() }

func (m *LogsViewModel) fetchLogsCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		entries, err := client.GetInferenceLogs("", "", "", 100)
		return logsLoadedMsg{entries: entries, err: err}
	}
}

// Update implements tea.Model.
func (m *LogsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.detail.SetVisible(vis)
		m.payloadDetail.SetVisible(vis)
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		if msg.Level == 1 {
			m.mode = logsViewList
		}
		if msg.Level == 2 && m.mode == logsViewPayload {
			m.closePayload()
		}
		return m, nil

	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		m.handleMouseWheel(msg)
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading || m.payloadLoading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case logsLoadedMsg:
		m.loading = false
		m.entries = msg.entries
		m.err = msg.err
		m.syncLogItems()
		return m, nil

	case logsNewEntryMsg:
		// Prepend new entry (newest first); cap at 1000.
		m.entries = append([]pkgClient.InferenceLogEntry{msg.entry}, m.entries...)
		if len(m.entries) > 1000 {
			m.entries = m.entries[:1000]
		}
		m.syncLogItems()
		if m.following && m.list.Cursor() == 0 {
			m.list.SetCursor(0)
		}
		if m.streamCh != nil {
			return m, waitForLogStream(m.streamCh)
		}
		return m, nil

	case logsStreamDoneMsg:
		m.following = false
		m.streamCh = nil
		m.cancelFollow = nil
		return m, nil

	case logsPayloadLoadedMsg:
		m.applyPayloadLoaded(msg)
		return m, nil

	case logsPayloadActionMsg:
		if msg.err != nil {
			m.payloadStatus = "Failed: " + msg.err.Error()
		} else {
			m.payloadStatus = msg.status
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	}

	return m, nil
}

func (m *LogsViewModel) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.mode == logsViewPayload {
		return m, m.handlePayloadKey(msg)
	}

	if m.mode == logsViewDetail {
		if key.Matches(msg, tui.LogsKeys.Payload) {
			return m, m.openPayload()
		}
		if m.detail.UpdateKey(msg) {
			return m, nil
		}
		if key.Matches(msg, tui.ListKeys.Back) {
			m.mode = logsViewList
			m.detail.Reset()
			return m, nil
		}
		return m, nil
	}

	if m.list.UpdateKey(msg) {
		return m, nil
	}

	switch {
	case key.Matches(msg, tui.LogsKeys.Enter):
		if m.list.Cursor() >= 0 {
			m.mode = logsViewDetail
		}
		return m, nil
	case key.Matches(msg, tui.LogsKeys.Payload):
		return m, m.openPayload()
	case key.Matches(msg, tui.LogsKeys.Refresh):
		m.loading = true
		client := m.client
		return m, func() tea.Msg {
			entries, err := client.GetInferenceLogs("", "", "", 100)
			return logsLoadedMsg{entries: entries, err: err}
		}
	case key.Matches(msg, tui.LogsKeys.Follow):
		if m.following {
			m.stopFollow()
			return m, nil
		}
		return m, m.startFollow()
	case key.Matches(msg, tui.ListKeys.Back):
		return m, tui.NavBack()
	case key.Matches(msg, tui.ListKeys.Quit):
		return m, tea.Quit
	}
	return m, nil
}

func (m *LogsViewModel) startFollow() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.following = true
	m.cancelFollow = cancel

	ch := make(chan tea.Msg, 64)
	m.streamCh = ch

	client := m.client
	go func() {
		defer close(ch)
		err := client.StreamInferenceLogs(ctx, "", func(entry pkgClient.InferenceLogEntry) {
			select {
			case ch <- logsNewEntryMsg{entry: entry}:
			default:
			}
		})
		if err != nil && ctx.Err() == nil {
			ch <- logsStreamDoneMsg{err: err}
		} else {
			ch <- logsStreamDoneMsg{}
		}
	}()

	return waitForLogStream(ch)
}

func (m *LogsViewModel) stopFollow() {
	m.following = false
	if m.cancelFollow != nil {
		m.cancelFollow()
		m.cancelFollow = nil
	}
}

// Cancel implements tui.Canceller. Root invokes this when the view leaves
// the stack so the SSE follow goroutine can shut down.
func (m *LogsViewModel) Cancel() { m.stopFollow() }

func waitForLogStream(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return logsStreamDoneMsg{}
		}
		return msg
	}
}

func (m *LogsViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == logsViewPayload {
			m.closePayload()
			return nil
		}
		if m.mode == logsViewDetail {
			m.mode = logsViewList
			m.detail.Reset()
			return nil
		}
		return tui.NavBack()
	}
	if m.mode != logsViewList {
		return nil
	}
	if enter, ok := m.list.Click(msg, shared.TuiTableClickY); ok && enter {
		m.mode = logsViewDetail
	}
	return nil
}

func (m *LogsViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	switch m.mode {
	case logsViewPayload:
		m.payloadDetail.UpdateWheel(msg)
	case logsViewDetail:
		m.detail.UpdateWheel(msg)
	case logsViewList:
		m.list.UpdateWheel(msg)
	}
}

// Breadcrumb implements tui.Breadcrumber.
func (m *LogsViewModel) Breadcrumb() []string {
	title := "Logs"
	if m.following {
		title = "Logs (live)"
	}
	if m.mode == logsViewPayload {
		// Pinned at open — a live-tailing list moves the cursor.
		if m.payloadModel != "" {
			return []string{title, m.payloadModel, "Payload"}
		}
		return []string{title, "Payload"}
	}
	if m.mode == logsViewDetail {
		c := m.list.Cursor()
		if c >= 0 && c < len(m.entries) {
			return []string{title, m.entries[c].Model}
		}
		return []string{title, "Details"}
	}
	return []string{title}
}

// View implements tea.Model.
func (m *LogsViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *LogsViewModel) viewContent() string {
	switch m.mode {
	case logsViewPayload:
		return m.viewPayload()
	case logsViewDetail:
		return m.viewDetail()
	}
	return m.viewList()
}

func (m *LogsViewModel) viewList() string {
	width := m.termWidth
	// A view with nothing in it still has to say how to get something:
	// without these the empty and error states offer only "back".
	backHint := tui.Hint(tui.ListKeys.Back, "back")
	if m.err != nil {
		backHint = tui.JoinHints(tui.Hint(tui.LogsKeys.Refresh, "retry"), backHint)
	}
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.entries) == 0,
		EmptyText: "No inference logs found",
		BackHint:  backHint,
		EmptyHint: tui.JoinHints(
			tui.Hint(tui.LogsKeys.Refresh, "refresh"),
			tui.Hint(tui.LogsKeys.Follow, "follow"),
			tui.Hint(tui.ListKeys.Back, "back")),
	}); done {
		return out
	}

	var b strings.Builder

	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
	start, end := m.list.VisibleRange()

	allRowStrs := make([][]string, len(m.entries))
	for i, entry := range m.entries {
		ts := entry.Timestamp.Local().Format("15:04:05")
		tps := "-"
		if entry.TokensPerSec > 0 {
			tps = fmt.Sprintf("%.0f", entry.TokensPerSec)
		}
		status := entry.Status
		if status == "error" {
			status = "ERR"
		} else {
			status = "OK"
		}
		cost := "-"
		if entry.Cost > 0 {
			cost = formatLogCost(entry.Cost)
		}
		app := entry.App
		if app == "" {
			app = "-"
		}
		allRowStrs[i] = []string{ts, entry.Model, app, fmt.Sprintf("%d/%d", entry.TokensIn, entry.TokensOut), formatLogLatency(entry.LatencyMs), tps, cost, status, shared.FormatPayloadMark(entry.PayloadAvailable)}
	}

	visibleEntries := m.entries[start:end]
	logRowStyles := make([]lipgloss.Style, len(visibleEntries))
	for i, entry := range visibleEntries {
		logRowStyles[i] = shared.StatusStyle(m.styles, entry.Status)
	}

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"TIME", "MODEL", "PROVIDER", "TOKENS", "LATENCY", "TPS", "COST", "STATUS", "PAYLOAD"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: 7,
		RowStyles: logRowStyles,
		AllRows:   allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	footerStatus := shared.TableStatus("Logs", len(m.entries), m.list.Offset(), vis)
	if m.following {
		footerStatus += "  LIVE"
	}

	hints := tui.JoinHints(
		tui.Hint(tui.ListKeys.Up, "navigate"),
		tui.Hint(tui.ListKeys.PageUp, "page"),
		tui.Hint(tui.LogsKeys.Enter, "details"),
		tui.Hint(tui.LogsKeys.Payload, "payload"),
		tui.Hint(tui.LogsKeys.Follow, "follow"),
		tui.Hint(tui.LogsKeys.Refresh, "refresh"),
		tui.Hint(tui.ListKeys.Back, "back"),
	)
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, footerStatus, hints))

	return b.String()
}

func (m *LogsViewModel) viewDetail() string {
	width := m.termWidth
	c := m.list.Cursor()
	if c < 0 || c >= len(m.entries) {
		return " No entry selected\n"
	}

	entry := m.entries[c]

	d := shared.NewDetail(m.styles, width)
	d.Field("ID", entry.ID)
	d.Field("Time", entry.Timestamp.Local().Format("2006-01-02 15:04:05.000"))
	d.Field("Model", entry.Model)
	d.Field("App", entry.App)
	d.Field("Node", entry.Node)
	d.Field("Routing", entry.RoutingDecision)
	d.Field("Type", entry.RequestType)
	if entry.Stream {
		d.Field("Streaming", "yes")
	}
	d.Field("Status", entry.Status)

	d.Section("Tokens")
	d.Field("Prompt", fmt.Sprintf("%d", entry.TokensIn))
	if entry.TokensCached > 0 {
		d.Field("  Cached", fmt.Sprintf("%d", entry.TokensCached))
	}
	d.Field("Completion", fmt.Sprintf("%d", entry.TokensOut))
	if entry.TokensReasoning > 0 {
		d.Field("  Reasoning", fmt.Sprintf("%d", entry.TokensReasoning))
	}
	if entry.TokensIn > 0 || entry.TokensOut > 0 {
		d.Field("Total", fmt.Sprintf("%d", entry.TokensIn+entry.TokensOut))
	}

	d.Section("Performance")
	if entry.TTFTMs > 0 {
		d.Field("TTFT", fmt.Sprintf("%.0fms", entry.TTFTMs))
	}
	d.Field("Latency", formatLogLatency(entry.LatencyMs))
	if entry.TokensPerSec > 0 {
		d.Field("Decode Speed", fmt.Sprintf("%.1f tok/s", entry.TokensPerSec))
	}
	if entry.Cost > 0 {
		d.Field("Cost", formatLogCost(entry.Cost))
	}

	if entry.Temperature != nil || entry.MaxTokens != nil || entry.TopP != nil {
		d.Section("Parameters")
		if entry.Temperature != nil {
			d.Field("Temperature", fmt.Sprintf("%.2f", *entry.Temperature))
		}
		if entry.MaxTokens != nil {
			d.Field("Max Tokens", fmt.Sprintf("%d", *entry.MaxTokens))
		}
		if entry.TopP != nil {
			d.Field("Top P", fmt.Sprintf("%.2f", *entry.TopP))
		}
	}

	if entry.Response != "" {
		d.Section("Response")
		d.Text(" ", shared.TruncateRunes(entry.Response, maxDetailResponseRunes))
	}

	if entry.SystemPrompt != "" {
		d.Section("System Prompt")
		d.Text(" ", shared.TruncateRunes(entry.SystemPrompt, maxDetailPromptRunes))
	}

	if len(entry.Messages) > 0 {
		d.Section("Messages")
		for i, msg := range entry.Messages {
			if msg.Role == "system" {
				continue
			}
			d.Field(fmt.Sprintf("[%d] %s", i, msg.Role),
				shared.TruncateRunes(msg.Content, maxDetailMessageRunes))
		}
	}

	if entry.ErrorType != "" {
		d.Section("Error")
		d.Field("Type", entry.ErrorType)
		d.Field("Message", entry.ErrorMessage)
	}

	allLines := strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
	footer := shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.ListKeys.Up, "scroll"),
		tui.Hint(tui.ListKeys.PageUp, "page"),
		tui.Hint(tui.LogsKeys.Payload, "payload"),
		tui.Hint(tui.ListKeys.Back, "back"),
	))
	scroll := m.detail.Scroll()
	result, _ := shared.RenderDetailView(m.styles, allLines, scroll, m.termHeight, footer, "", 0)
	return result
}

func formatLogCost(cost float64) string {
	if cost < 0.001 {
		return fmt.Sprintf("$%.4f", cost)
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.3f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func formatLogLatency(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}
