package views

import (
	"context"
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// nodeItem wraps shared.NodeInfo to satisfy tui.Item so the tui.List knows the
// item count for cursor/offset/wheel math and separator support.
type nodeItem struct{ shared.NodeInfo }

func (n nodeItem) Title() string       { return n.Name }
func (n nodeItem) Description() string { return n.IPAddress }
func (n nodeItem) FilterValue() string { return n.Name + " " + n.IPAddress }

type nodesLoadedMsg struct {
	nodes []shared.NodeInfo
	err   error
}

type nodeDetailMsg struct {
	ctx  context.Context
	name string
	node shared.NodeInfo
	err  error
}

// pairAcceptedMsg is the response to an admin-POST to
// /zzrouter/v1/cluster/pairing/accept triggered from the nodes view.
// Result is nil when err != nil; when successful, carries the
// worker's name + fingerprint for a visual cross-check in the
// status bar.
type pairAcceptedMsg struct {
	result *pkgClient.PairAcceptResult
	err    error
}

type nodeRemovedMsg struct {
	address string
	err     error
}

type nodesViewMode int

const (
	nodesViewList nodesViewMode = iota
	nodesViewDetail
	nodesViewPair
)

var nodesKeys = struct {
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Home     key.Binding
	End      key.Binding
	Enter    key.Binding
	Pair     key.Binding
	Delete   key.Binding
	Refresh  key.Binding
	Back     key.Binding
	Quit     key.Binding
}{
	Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "navigate")),
	PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
	PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
	Home:     key.NewBinding(key.WithKeys("home"), key.WithHelp("Home", "top")),
	End:      key.NewBinding(key.WithKeys("end"), key.WithHelp("End", "bottom")),
	Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "details")),
	Pair:     key.NewBinding(key.WithKeys("p", "P"), key.WithHelp("P", "pair")),
	Delete:   key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("D", "remove")),
	Refresh:  key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "refresh")),
	Back:     key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
	Quit:     key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
}

func nodesListHints() string {
	return tui.JoinHints(
		tui.Hint(nodesKeys.Up, "navigate"),
		tui.Hint(nodesKeys.Enter, "details"),
		tui.Hint(nodesKeys.Pair, "pair"),
		tui.Hint(nodesKeys.Delete, "remove"),
		tui.Hint(nodesKeys.Refresh, "refresh"),
		tui.Hint(nodesKeys.Back, "back"),
	)
}

type NodesViewModel struct {
	tui.ViewContext
	help          tui.Help
	detailContext context.Context
	detailCancel  context.CancelFunc
	detailName    string

	client        *pkgClient.Client
	styles        ui.Styles
	nodes         []shared.NodeInfo
	list          tui.List
	detail        tui.Detail
	confirm       tui.Confirm
	termWidth     int
	termHeight    int
	loading       bool
	err           error
	mode          nodesViewMode
	detailNode    *shared.NodeInfo
	detailLoading bool
	loadTick      int

	pairInput       textinput.Model
	pairURL         textinput.Model
	pairFingerprint textinput.Model

	confirmPending bool
	confirmTarget  string

	statusMsg string
	statusErr bool
}

func NewNodesViewModel(client *pkgClient.Client, styles ui.Styles) *NodesViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up:       nodesKeys.Up,
		Down:     nodesKeys.Down,
		PageUp:   nodesKeys.PageUp,
		PageDown: nodesKeys.PageDown,
		Home:     nodesKeys.Home,
		End:      nodesKeys.End,
	})
	detail := tui.NewDetail()
	detail.SetKeys(tui.NavigationKeys{
		Up:       nodesKeys.Up,
		Down:     nodesKeys.Down,
		PageUp:   nodesKeys.PageUp,
		PageDown: nodesKeys.PageDown,
		Home:     nodesKeys.Home,
		End:      nodesKeys.End,
	})
	return &NodesViewModel{
		client:          client,
		styles:          styles,
		loading:         true,
		list:            list,
		detail:          detail,
		confirm:         tui.NewConfirm(),
		pairInput:       newFormInput("ABCD-EFGH-IJKL-MNOP (from worker)", 32),
		pairURL:         newFormInput("https://worker:9091", 256),
		pairFingerprint: newFormInput("sha256:... (from worker)", 80),
	}
}

// Init implements tea.Model.
func (m *NodesViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchNodesCmd(false), shared.SpinnerTickCmd())
}

// Refresh implements tui.Refresher: reload when a child view pops so node
// health and provider state reflect anything the child changed. refresh=true
// makes the server re-probe workers rather than serve its cache.
func (m *NodesViewModel) Refresh() tea.Cmd { return m.fetchNodesCmd(true) }

func (m *NodesViewModel) fetchNodesCmd(refresh bool) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		nodes, err := shared.FetchNodes(client, "all", refresh)
		return nodesLoadedMsg{nodes: nodes, err: err}
	}
}

// Update implements tea.Model.
func (m *NodesViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.help.Update(msg, m.mode == nodesViewList || m.mode == nodesViewDetail) {
		return m, nil
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.detail.SetVisible(vis)
		return m, nil

	case pairAcceptedMsg:
		m.loading = false
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.statusMsg = fmt.Sprintf("Paired %s (%s)", msg.result.NodeName, msg.result.ShortForm)
		m.statusErr = false
		m.loading = true
		client := m.client
		return m, tea.Batch(func() tea.Msg {
			nodes, err := shared.FetchNodes(client, "all", true)
			return nodesLoadedMsg{nodes: nodes, err: err}
		}, shared.SpinnerTickCmd())

	case nodeRemovedMsg:
		m.loading = false
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			m.statusErr = true
			return m, nil
		}
		m.statusMsg = fmt.Sprintf("Node %s removed", msg.address)
		m.statusErr = false
		m.loading = true
		client := m.client
		return m, tea.Batch(func() tea.Msg {
			nodes, err := shared.FetchNodes(client, "all", true)
			return nodesLoadedMsg{nodes: nodes, err: err}
		}, shared.SpinnerTickCmd())

	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		m.handleMouseWheel(msg)
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case nodesLoadedMsg:
		m.loading = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.nodes) > 0 {
			m.statusMsg = fmt.Sprintf("Refresh failed: %v", msg.err)
			return m, nil
		}
		m.nodes = msg.nodes
		m.err = msg.err
		items := make([]tui.Item, len(msg.nodes))
		for i, n := range msg.nodes {
			items[i] = nodeItem{n}
		}
		m.list.SetItems(items)
		return m, nil

	case nodeDetailMsg:
		if msg.ctx == nil || msg.ctx != m.detailContext || msg.ctx.Err() != nil || m.mode != nodesViewDetail || msg.name != m.detailName {
			return m, nil
		}
		m.detailLoading = false
		if msg.err == nil {
			m.detailNode = &msg.node
		}
		return m, nil

	case tea.PasteMsg:
		// Paste support for the pair-code field. Textinput's stock
		// KeyPressMsg handler ignores PasteMsg, so we append the pasted
		// content manually. Trim whitespace so a trailing newline from
		// the system clipboard doesn't bloat the code.
		if m.mode == nodesViewPair {
			input := m.activePairInput()
			input.SetValue(input.Value() + strings.TrimSpace(msg.Content))
		}
		return m, nil

	case tea.KeyPressMsg:
		if m.mode == nodesViewDetail {
			if m.detail.UpdateKey(msg) {
				return m, nil
			}
			if key.Matches(msg, nodesKeys.Back, nodesKeys.Quit) {
				m.mode = nodesViewList
				m.detailNode = nil
				if m.detailCancel != nil {
					m.detailCancel()
				}
				m.detail.Reset()
				return m, nil
			}
			return m, nil
		}

		if m.mode == nodesViewPair {
			// Input.Focused() == false means we're showing the
			// post-submit result inside the popup. Any key dismisses
			// the popup and returns to the refreshed list.
			if !m.pairFormFocused() {
				m.mode = nodesViewList
				m.statusMsg = ""
				m.statusErr = false
				return m, nil
			}
			switch {
			case msg.String() == "esc":
				m.mode = nodesViewList
				m.blurPairForm()
				m.statusMsg = ""
				return m, nil
			case msg.String() == "tab" || msg.String() == "shift+tab" || (key.Matches(msg, nodesKeys.Enter) && !m.pairInput.Focused()):
				current := 0
				if m.pairFingerprint.Focused() {
					current = 1
				} else if m.pairInput.Focused() {
					current = 2
				}
				step := 1
				if msg.String() == "shift+tab" {
					step = 2
				}
				m.blurPairForm()
				inputs := []*textinput.Model{&m.pairURL, &m.pairFingerprint, &m.pairInput}
				return m, inputs[(current+step)%3].Focus()
			case key.Matches(msg, nodesKeys.Enter):
				code := strings.TrimSpace(m.pairInput.Value())
				workerURL, workerFingerprint := strings.TrimSpace(m.pairURL.Value()), strings.TrimSpace(m.pairFingerprint.Value())
				if code == "" || workerURL == "" || workerFingerprint == "" {
					m.statusMsg = "Worker URL, fingerprint and code are required"
					m.statusErr = true
					return m, nil
				}
				// Stay in pair mode so the "Accepting…" + eventual
				// success/error renders inside the popup, not in the
				// list view underneath.
				m.blurPairForm()
				m.statusMsg = "Accepting pairing code..."
				m.statusErr = false
				client := m.client
				return m, func() tea.Msg {
					res, err := client.AcceptPairingCode(code, workerURL, workerFingerprint)
					return pairAcceptedMsg{result: res, err: err}
				}
			}
			var cmd tea.Cmd
			input := m.activePairInput()
			*input, cmd = input.Update(msg)
			return m, cmd
		}

		if m.confirmPending {
			switch m.confirm.UpdateKey(msg) {
			case tui.ConfirmYes:
				addr := m.confirmTarget
				m.confirmPending = false
				m.confirmTarget = ""
				m.statusMsg = fmt.Sprintf("Removing %s...", addr)
				m.statusErr = false
				client := m.client
				return m, func() tea.Msg {
					err := client.RemoveClusterEndpoint(addr)
					return nodeRemovedMsg{address: addr, err: err}
				}
			case tui.ConfirmNo, tui.ConfirmCancelled:
				m.confirmPending = false
				m.confirmTarget = ""
				m.statusMsg = ""
				return m, nil
			}
			return m, nil
		}

		if m.list.UpdateKey(msg) {
			return m, nil
		}

		switch {
		case key.Matches(msg, nodesKeys.Enter):
			c := m.list.Cursor()
			if c >= 0 && c < len(m.nodes) {
				node := m.nodes[c]
				if shared.NodeDisplayStatus(node.HealthStatus) == "offline" {
					return m, nil
				}
				m.mode = nodesViewDetail
				m.detailLoading = true
				m.detailNode = nil
				if m.detailCancel != nil {
					m.detailCancel()
				}
				m.detail.Reset()
				return m, m.loadNodeDetail(node.Name)
			}
			return m, nil
		case key.Matches(msg, nodesKeys.Pair):
			m.mode = nodesViewPair
			m.pairInput.SetValue("")
			m.pairURL.SetValue("")
			m.pairFingerprint.SetValue("")
			m.blurPairForm()
			m.pairURL.Focus()
			m.statusMsg = ""
			return m, nil
		case key.Matches(msg, nodesKeys.Delete):
			c := m.list.Cursor()
			if c < 0 || c >= len(m.nodes) {
				return m, nil
			}
			node := m.nodes[c]
			if strings.EqualFold(node.ClusterRole, "coordinator") {
				m.statusMsg = "Coordinator cannot be removed"
				m.statusErr = true
				return m, nil
			}
			addr := nodeRemoveAddress(node)
			if addr == "" {
				m.statusMsg = "Node has no known address"
				m.statusErr = true
				return m, nil
			}
			m.confirmPending = true
			m.confirmTarget = addr
			m.statusMsg = ""
			return m, nil
		case key.Matches(msg, nodesKeys.Refresh):
			m.loading = true
			client := m.client
			return m, func() tea.Msg {
				nodes, err := shared.FetchNodes(client, "all", true)
				return nodesLoadedMsg{nodes: nodes, err: err}
			}
		case key.Matches(msg, nodesKeys.Back):
			return m, tui.NavBack()
		case key.Matches(msg, nodesKeys.Quit):
			return m, tea.Quit
		}

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		m.navigateToBreadcrumb(msg.Level)
		return m, nil
	}

	return m, nil
}

func (m *NodesViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == nodesViewDetail {
			m.mode = nodesViewList
			m.detail.Reset()
		} else {
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != nodesViewList {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok || !enter {
		return nil
	}
	node := m.nodes[m.list.Cursor()]
	if shared.NodeDisplayStatus(node.HealthStatus) != "online" {
		return nil
	}
	m.mode = nodesViewDetail
	m.detailLoading = true
	m.detailNode = nil
	m.detail.Reset()
	return m.loadNodeDetail(node.Name)
}

func (m *NodesViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	if m.mode == nodesViewDetail {
		m.detail.UpdateWheel(msg)
		return
	}
	if m.mode == nodesViewList {
		m.list.UpdateWheel(msg)
	}
}

func fetchNodeDetail(ctx context.Context, client *pkgClient.Client, name string) nodeDetailMsg {
	resp, err := client.GetNodeContext(ctx, name)
	if err != nil {
		return nodeDetailMsg{ctx: ctx, name: name, err: err}
	}

	node := shared.NodeInfo{
		Name:          shared.GetStrField(resp, "name"),
		IPAddress:     shared.GetStrField(resp, "ip_address"),
		ClusterRole:   shared.GetStrField(resp, "cluster_role"),
		HealthStatus:  shared.GetStrField(resp, "health_status"),
		OS:            shared.GetStrField(resp, "os"),
		Version:       shared.GetStrField(resp, "version"),
		Disk:          shared.FormatDisk(resp),
		Memory:        shared.FormatMemory(resp),
		GPU:           shared.FormatGPU(resp),
		GPUDesc:       shared.FormatGPUDesc(resp),
		GPUs:          shared.ParseGPUs(resp),
		UptimeSeconds: shared.GetIntField(resp, "uptime_seconds"),
		Address:       shared.GetStrField(resp, "address"),
		Providers:     shared.ParseNodeProviders(resp),
		LastError:     shared.GetStrField(resp, "last_error"),
	}

	return nodeDetailMsg{ctx: ctx, name: name, node: node}
}

// formatVRAM renders a card's memory for the node detail view. A nil
// freeGB means the node never measured this card, which is not the same
// as a card with nothing left, so the two render differently. The labels
// spell out which figure is which: "0 / 96" invites reading the first
// number as used rather than free.
func formatVRAM(freeGB *float64, totalGB float64) string {
	if totalGB <= 0 {
		return "size N/A"
	}
	if freeGB == nil {
		return fmt.Sprintf("%.0f GB total, free N/A", totalGB)
	}
	return fmt.Sprintf("%.0f GB free / %.0f GB total", *freeGB, totalGB)
}

func (m *NodesViewModel) pairFormFocused() bool {
	return m.pairURL.Focused() || m.pairFingerprint.Focused() || m.pairInput.Focused()
}
func (m *NodesViewModel) blurPairForm() {
	m.pairURL.Blur()
	m.pairFingerprint.Blur()
	m.pairInput.Blur()
}
func (m *NodesViewModel) activePairInput() *textinput.Model {
	if m.pairURL.Focused() {
		return &m.pairURL
	}
	if m.pairFingerprint.Focused() {
		return &m.pairFingerprint
	}
	return &m.pairInput
}

func (m *NodesViewModel) loadNodeDetail(name string) tea.Cmd {
	if m.detailCancel != nil {
		m.detailCancel()
	}
	ctx, cancel := context.WithCancel(m.Context())
	m.detailContext, m.detailCancel, m.detailName = ctx, cancel, name
	client := m.client
	return func() tea.Msg { return fetchNodeDetail(ctx, client, name) }
}

// Cancel releases the node detail request when the view leaves the stack.
func (m *NodesViewModel) Cancel() {
	m.ViewContext.Cancel()
	if m.detailCancel != nil {
		m.detailCancel()
	}
}
