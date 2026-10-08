package views

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ============================================================================
// Deployments view — first multi-row jobstream consumer
// ============================================================================
//
// Lists active /deployments, one row per (deployment, node). Each row
// holds its own *jobstream.Row keyed by JobID and updates live as
// FrameMsgs arrive. Per-row cancel via "x" on the focused row;
// view-level cancel cascades to every row via Cancel().
//
// Compared to the wizard's singleton row (one *jobstream.Row field),
// this view is the first true N>1 consumer — rows live in a map keyed
// by JobID and FrameMsg routing is lookup-by-ID.

// ============================================================================
// Messages
// ============================================================================

type deploysLoadedMsg struct {
	err error
}

// deploysSubscribedMsg carries the rows + initial Next cmds from a
// fresh fetch. Park rows on the model and Batch the next cmds so all
// streams begin pumping in parallel.
type deploysSubscribedMsg struct {
	rows  map[string]*jobstream.Row // keyed by JobID
	order []string                  // stable render order
	cmds  []tea.Cmd
	meta  map[string]jobMeta // static per-job context, keyed by JobID
}

// ============================================================================
// Keys
// ============================================================================

var deploysKeys = struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Cancel   key.Binding
	Refresh  key.Binding
	Back     key.Binding
}{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j")),
	PageUp:   key.NewBinding(key.WithKeys("pgup")),
	PageDown: key.NewBinding(key.WithKeys("pgdown")),
	Home:     key.NewBinding(key.WithKeys("home")),
	End:      key.NewBinding(key.WithKeys("end")),
	Cancel:   key.NewBinding(key.WithKeys("x", "X"), key.WithHelp("X", "cancel row")),
	Refresh:  key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
}

func deploysHints() string {
	return tui.JoinHints(
		tui.Hint(deploysKeys.Up, "navigate"),
		tui.Hint(deploysKeys.Cancel, "cancel row"),
		tui.Hint(deploysKeys.Refresh, "refresh"),
		tui.Hint(deploysKeys.Back, "back"),
	)
}

// ============================================================================
// Model
// ============================================================================

// deployRow is the per-row UI state. The jobstream.Row holds the live
// SSE state; this struct adds the static metadata (model, node name)
// needed for table rendering.
type deployRow struct {
	JobID  string
	Kind   string // install | upgrade | uninstall | download
	Model  string // the model or provider the job acts on
	Node   string
	stream *jobstream.Row
}

// DeploysViewModel is the top-of-stack model for the multi-row deploy
// streaming view. Implements tui.Canceller so Root teardown cancels
// every active row's subscription.
type DeploysViewModel struct {
	tui.ViewContext

	client *pkgClient.Client
	styles ui.Styles

	loading      bool
	loadTick     int
	err          error
	actionStatus string

	// rows keyed by JobID; order preserves render position across
	// re-fetches (entries are appended on first sight, kept on refresh).
	rows   map[string]*deployRow
	order  []string
	cursor int

	termWidth, termHeight int
}

func NewDeploysViewModel(client *pkgClient.Client, styles ui.Styles) *DeploysViewModel {
	return &DeploysViewModel{
		client:  client,
		styles:  styles,
		loading: true,
		rows:    map[string]*deployRow{},
	}
}

func (m *DeploysViewModel) Breadcrumb() []string { return []string{"Menu", "Activity"} }

// Cancel satisfies tui.Canceller. Tears down every active row's stream
// before propagating to the view's ViewContext. Both calls are needed:
// Row.Cancel runs the drain goroutine that unblocks any pending
// channel sends; ViewContext.Cancel propagates to producers' ctx.
func (m *DeploysViewModel) Cancel() {
	for _, r := range m.rows {
		if r.stream != nil {
			r.stream.Cancel()
		}
	}
	m.ViewContext.Cancel()
}

func (m *DeploysViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchAndSubscribe(), shared.SpinnerTickCmd())
}

// Refresh implements tui.Refresher: pick up jobs started while a child view
// was on top. fetchAndSubscribe skips jobs already streaming, so re-running
// it never opens a duplicate SSE connection.
func (m *DeploysViewModel) Refresh() tea.Cmd { return m.fetchAndSubscribe() }

// fetchAndSubscribe fetches every in-flight job across the cluster and
// opens a jobstream subscription per job. Returns a deploysSubscribedMsg
// with the parked rows + the initial Next cmds the Update loop must Batch.
//
// This reads /jobs rather than /deployments so installs, upgrades and
// uninstalls appear alongside downloads. Deployment state itself belongs
// to the Model Deployments view, which renders transfer rate and ETA that
// a job snapshot does not carry; this view answers "what is running right
// now, and can I stop it".
func (m *DeploysViewModel) fetchAndSubscribe() tea.Cmd {
	client := m.client
	ctx := m.Context()
	// Snapshot on the Update goroutine; the cmd below runs off-loop and
	// must not read m.rows directly.
	tracked := make(map[string]struct{}, len(m.rows))
	for id := range m.rows {
		tracked[id] = struct{}{}
	}
	return func() tea.Msg {
		jobEvents, err := shared.FetchAllJobs(ctx, client)
		if err != nil {
			return deploysLoadedMsg{err: err}
		}
		rows := map[string]*jobstream.Row{}
		meta := map[string]jobMeta{}
		var order []string
		var cmds []tea.Cmd
		for _, j := range jobEvents {
			if j.JobID == "" || isTerminalPhase(j.Phase) {
				continue
			}
			if _, dup := rows[j.JobID]; dup {
				continue
			}
			// Already streaming: re-subscribing would open a second SSE
			// connection whose Row the parking loop discards, orphaning a
			// producer that blocks forever on a full channel and leaving
			// two readers racing the same stream.
			if _, live := tracked[j.JobID]; live {
				continue
			}
			row, next := jobstream.Subscribe(ctx, client, j.JobID, j.Node)
			rows[j.JobID] = row
			meta[j.JobID] = jobMeta{Kind: j.Kind, Label: jobLabel(j)}
			order = append(order, j.JobID)
			cmds = append(cmds, next)
		}
		return deploysSubscribedMsg{rows: rows, order: order, cmds: cmds, meta: meta}
	}
}

// deploysCancelMsg reports the outcome of a per-row cancel so a failure
// is surfaced instead of being dropped on the floor.
type deploysCancelMsg struct{ err error }

// cancelJobByKind routes a cancel to the call that actually stops the
// work. Downloads are the exception: the transfer goroutine watches the
// download tracker's cancel func and never its job context, so
// DELETE /jobs/:id answers 202 while the bytes keep flowing. Those go
// through the owning deployment instead, which is resolved by job ID.
func cancelJobByKind(ctx context.Context, client *pkgClient.Client, kind, jobID string) error {
	if kind != string(jobs.KindDownload) {
		return client.CancelJob(ctx, jobID)
	}
	deployments, err := client.ListDeployments()
	if err != nil {
		return fmt.Errorf("resolve deployment for download: %w", err)
	}
	for _, d := range deployments {
		if d == nil {
			continue
		}
		for _, n := range d.Nodes {
			if n.JobID == jobID {
				return client.CancelDeploymentNode(d.ID, n.Node)
			}
		}
	}
	return fmt.Errorf("no active deployment owns job %s", jobID)
}

// jobMeta is the static per-job context a jobstream frame does not carry:
// the job kind and a human label for the thing being acted on.
type jobMeta struct {
	Kind  string
	Label string
}

// isTerminalPhase reports whether a job has already settled. The registry
// retains finished jobs so agents can read outcomes, but an activity view
// that lists them would fill with history.
func isTerminalPhase(phase string) bool {
	switch phase {
	case "done", "failed", "cancelled", "canceled", "error":
		return true
	}
	return false
}

// jobLabel picks the best available name for what a job is acting on.
// Download jobs carry a model in Meta; provider jobs carry a provider.
// Falling back to the job ID keeps a row addressable when neither is set.
func jobLabel(j pkgClient.JobEvent) string {
	for _, key := range []string{"model", "provider", "name"} {
		if v, ok := j.Meta[key].(string); ok && v != "" {
			return v
		}
	}
	return j.JobID
}

// ============================================================================
// Update
// ============================================================================

func (m *DeploysViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m, m.handleKey(msg)

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case deploysCancelMsg:
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("✗ Cancel failed: %v", msg.err)
		}
		return m, nil

	case deploysLoadedMsg:
		// Fetch error path: the subscribe cmd never built rows.
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

	case deploysSubscribedMsg:
		m.loading = false
		// A prior failure otherwise pins the view on the error screen:
		// viewBody returns early on m.err, so every later refresh would
		// render "Error: ..." with no rows until the view was popped.
		m.err = nil
		// The job snapshot already carries kind and target, so rows render
		// fully on first paint — no placeholder and no second round-trip.
		for _, jobID := range msg.order {
			stream := msg.rows[jobID]
			if stream == nil {
				continue
			}
			if _, exists := m.rows[jobID]; !exists {
				md := msg.meta[jobID]
				m.rows[jobID] = &deployRow{
					JobID:  jobID,
					Node:   stream.Node,
					Kind:   md.Kind,
					Model:  md.Label,
					stream: stream,
				}
				m.order = append(m.order, jobID)
			}
		}
		return m, tea.Batch(msg.cmds...)

	case jobstream.FrameMsg:
		row, ok := m.rows[msg.JobID]
		if !ok || row.stream == nil {
			return m, nil
		}
		if msg.Done {
			row.stream.Done = true
			if msg.Err != nil {
				row.stream.Err = msg.Err
			} else {
				row.stream.Latest = msg.Event
				row.stream.Phase = msg.Event.Phase
			}
			return m, nil
		}
		row.stream.Latest = msg.Event
		row.stream.Phase = msg.Event.Phase
		return m, jobstream.Next(row.stream)
	}
	return m, nil
}

// ============================================================================
// Key handling
// ============================================================================

func (m *DeploysViewModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, deploysKeys.Back):
		return tui.NavBack()

	case key.Matches(msg, deploysKeys.Up):
		if m.cursor > 0 {
			m.cursor--
		}

	case key.Matches(msg, deploysKeys.Down):
		if m.cursor < len(m.order)-1 {
			m.cursor++
		}

	case key.Matches(msg, deploysKeys.Home):
		m.cursor = 0

	case key.Matches(msg, deploysKeys.End):
		if len(m.order) > 0 {
			m.cursor = len(m.order) - 1
		}

	case key.Matches(msg, deploysKeys.Cancel):
		// Index the same order the table renders. orderSorted sinks Done
		// rows to the bottom, so indexing m.order here would cancel a
		// different job than the highlighted one — now that this view
		// lists run jobs, that mistake kills a live model instance.
		sorted := m.orderSorted()
		if m.cursor < 0 || m.cursor >= len(sorted) {
			return nil
		}
		jobID := sorted[m.cursor]
		row, ok := m.rows[jobID]
		if !ok || row.stream == nil || row.stream.Done {
			return nil
		}
		client := m.client
		kind := row.Kind
		m.actionStatus = fmt.Sprintf("Cancelling %s on %s...", row.Model, row.Node)
		// Detached ctx so the DELETE survives a near-simultaneous view
		// pop (matches the CLI watch.go pattern).
		ctx := m.Context()
		return func() tea.Msg {
			return deploysCancelMsg{err: cancelJobByKind(ctx, client, kind, jobID)}
		}

	case key.Matches(msg, deploysKeys.Refresh):
		// Re-fetch /deployments and subscribe to any new (job_id) entries
		// that aren't already streaming. Existing rows keep their state.
		m.loading = true
		m.actionStatus = ""
		return tea.Batch(m.fetchAndSubscribe(), shared.SpinnerTickCmd())
	}
	return nil
}

// ============================================================================
// View
// ============================================================================

func (m *DeploysViewModel) View() tea.View {
	// RenderChildView (not a bare tea.NewView) so this view keeps AltScreen
	// set; without it the view renders inline and leaves frames in scrollback.
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewBody())
}

func (m *DeploysViewModel) viewBody() string {
	if m.loading && len(m.rows) == 0 {
		return fmt.Sprintf("\n  %s Loading deployments...\n", shared.SpinnerFrame(m.loadTick))
	}
	if m.err != nil {
		return "\n" + shared.WrapIndent(
			fmt.Sprintf("%s Error: %v", m.styles.Error.Render("✗"), m.err),
			"  ", m.termWidth) + "\n"
	}
	if len(m.order) == 0 {
		return "\n  No jobs running. Press R to refresh, Esc to go back.\n"
	}

	width := m.termWidth
	if width <= 0 {
		width = 100
	}

	var b strings.Builder
	b.WriteString("\n")
	colWidths := m.deploysColWidths(width)
	b.WriteString(m.headerRow(colWidths))
	b.WriteString("\n")
	b.WriteString(m.separator(width))
	b.WriteString("\n")
	for i, jobID := range m.orderSorted() {
		row, ok := m.rows[jobID]
		if !ok {
			continue
		}
		b.WriteString(m.renderRow(row, i == m.cursor, colWidths))
		b.WriteString("\n")
	}
	if m.actionStatus != "" {
		b.WriteString("\n  " + m.styles.Help.Render(m.actionStatus) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(shared.RenderViewFooter(m.styles, width, deploysHints()))
	return b.String()
}

// orderSorted returns the JobID slice with terminated rows pushed to
// the bottom — keeps the active streams visually grouped at the top
// without rebuilding the order vector on every transition.
func (m *DeploysViewModel) orderSorted() []string {
	out := make([]string, len(m.order))
	copy(out, m.order)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := m.rows[out[i]], m.rows[out[j]]
		if ri == nil || rj == nil || ri.stream == nil || rj.stream == nil {
			return false
		}
		return !ri.stream.Done && rj.stream.Done
	})
	return out
}

// deploysHeaders labels the columns; deploysColMins floors each one so the
// table keeps its shape while jobs are still reporting empty cells.
var (
	deploysHeaders = []string{"KIND", "TARGET", "NODE", "PHASE", "PERCENT", "BYTES"}
	deploysColMins = []int{6, 10, 8, 8, 8, 0}
)

// deploysColWidths sizes the columns to the jobs actually on screen and to the
// terminal, so a long model name is not clipped at a fixed 24 cells on a wide
// window. deploysRowPad is the single space between columns.
const deploysRowPad = 1

func (m *DeploysViewModel) deploysColWidths(width int) []int {
	cells := make([][]string, 0, len(m.order))
	for _, jobID := range m.order {
		if row, ok := m.rows[jobID]; ok {
			cells = append(cells, m.rowCells(row))
		}
	}
	return shared.MeasureColumns(shared.ColumnLayout{
		Headers: deploysHeaders,
		Rows:    cells,
		Mins:    deploysColMins,
		// 2 = the row's leading indent, which is not part of any column.
		Width: width - 2,
		Pad:   deploysRowPad,
		Flex:  1, // TARGET holds the model name, the longest free-form cell
	})
}

func (m *DeploysViewModel) headerRow(colWidths []int) string {
	return m.styles.Help.Render("  " + padCells(deploysHeaders, colWidths))
}

// padCells lays cells out in fixed columns, truncating any that overflow.
// Widths are display cells, so emoji and CJK do not shift the columns the way
// fmt's byte-counting %-*s does.
func padCells(cells []string, colWidths []int) string {
	var b strings.Builder
	for i, cell := range cells {
		w := colWidths[i]
		cell = shared.TruncateText(cell, w-deploysRowPad)
		b.WriteString(cell + strings.Repeat(" ", max(w-lipgloss.Width(cell), 0)))
	}
	return strings.TrimRight(b.String(), " ")
}

func (m *DeploysViewModel) separator(width int) string {
	if width < 4 {
		width = 4
	}
	return "  " + strings.Repeat("─", width-4)
}

// rowCells returns a job's untruncated cell values in column order, so the
// same values feed both column measurement and rendering.
func (m *DeploysViewModel) rowCells(r *deployRow) []string {
	if r.stream == nil {
		return nil
	}
	kind := r.Kind
	if kind == "" {
		kind = "job"
	}

	phase := r.stream.Phase
	if phase == "" {
		phase = "pending"
	}
	if r.stream.Done {
		if r.stream.Err != nil {
			phase = "failed"
		} else if phase != "failed" {
			phase = "done"
		}
	}

	pct := ""
	if r.stream.Latest.Percent > 0 || r.stream.Done {
		pct = fmt.Sprintf("%d%%", r.stream.Latest.Percent)
	}

	bytesStr := ""
	if b := r.stream.Latest.Bytes; b != nil && b.Total > 0 {
		bytesStr = fmt.Sprintf("%s / %s",
			shared.FormatSize(b.Done), shared.FormatSize(b.Total))
	}
	// events_dropped markers are suppressed from the FrameMsg pipeline
	// by jobstream; the Row keeps an atomic counter of how many slipped
	// past on the producer side (ring overflow). Surface non-zero as a
	// small warning annotation so an operator knows their view isn't
	// the complete history.
	if missed := r.stream.Dropped.Load(); missed > 0 {
		bytesStr = fmt.Sprintf("%s ⚠ %d missed", bytesStr, missed)
	}

	return []string{kind, r.Model, r.Node, phase, pct, bytesStr}
}

func (m *DeploysViewModel) renderRow(r *deployRow, focused bool, colWidths []int) string {
	cells := m.rowCells(r)
	if cells == nil {
		return ""
	}
	line := "  " + padCells(cells, colWidths)

	switch {
	case focused:
		return m.styles.Normal.Bold(true).Render("▌ " + line[2:])
	case r.stream.Err != nil:
		return m.styles.Error.Render(line)
	case r.stream.Done:
		return m.styles.Success.Render(line)
	}
	return m.styles.Normal.Render(line)
}
