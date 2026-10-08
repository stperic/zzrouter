package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/ui/form"
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

	pairInput textinput.Model

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
		client:    client,
		styles:    styles,
		loading:   true,
		list:      list,
		detail:    detail,
		confirm:   tui.NewConfirm(),
		pairInput: newFormInput("ABCD-EFGH-IJKL-MNOP (from worker)", 32),
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
			m.pairInput.SetValue(m.pairInput.Value() + strings.TrimSpace(msg.Content))
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
				m.detail.Reset()
				return m, nil
			}
			return m, nil
		}

		if m.mode == nodesViewPair {
			// Input.Focused() == false means we're showing the
			// post-submit result inside the popup. Any key dismisses
			// the popup and returns to the refreshed list.
			if !m.pairInput.Focused() {
				m.mode = nodesViewList
				m.statusMsg = ""
				m.statusErr = false
				return m, nil
			}
			switch {
			case key.Matches(msg, nodesKeys.Back):
				m.mode = nodesViewList
				m.pairInput.Blur()
				m.statusMsg = ""
				return m, nil
			case key.Matches(msg, nodesKeys.Enter):
				code := strings.TrimSpace(m.pairInput.Value())
				if code == "" {
					m.statusMsg = "Pairing code is required"
					m.statusErr = true
					return m, nil
				}
				// Stay in pair mode so the "Accepting…" + eventual
				// success/error renders inside the popup, not in the
				// list view underneath.
				m.pairInput.Blur()
				m.statusMsg = "Accepting pairing code..."
				m.statusErr = false
				client := m.client
				return m, func() tea.Msg {
					res, err := client.AcceptPairingCode(code)
					return pairAcceptedMsg{result: res, err: err}
				}
			}
			var cmd tea.Cmd
			m.pairInput, cmd = m.pairInput.Update(msg)
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
				m.detail.Reset()
				client := m.client
				return m, func() tea.Msg {
					return fetchNodeDetail(client, node.Name)
				}
			}
			return m, nil
		case key.Matches(msg, nodesKeys.Pair):
			m.mode = nodesViewPair
			m.pairInput.SetValue("")
			m.pairInput.Focus()
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
	client := m.client
	return func() tea.Msg {
		return fetchNodeDetail(client, node.Name)
	}
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

// View implements tea.Model.
func (m *NodesViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *NodesViewModel) viewContent() string {
	if m.mode == nodesViewDetail {
		return m.viewDetail(m.termWidth)
	}
	if m.mode == nodesViewPair {
		return m.viewPairFormOverlay(m.termWidth, m.termHeight)
	}
	return m.viewList(m.termWidth)
}

func (m *NodesViewModel) viewPairFormOverlay(width, height int) string {
	var popup strings.Builder
	// Accent (no horizontal padding) for the title so it aligns flush
	// with the rest of the popup body — styles.Title has Padding(0, 1)
	// which visually offsets it by one character.
	popup.WriteString(m.styles.Accent.Render("Accept worker pairing") + "\n\n")
	// HintsBar (Subtext) for supporting copy + action hints — a shade
	// lighter than Help (Overlay) so the secondary text is readable
	// against the popup surface.
	popup.WriteString(m.styles.HintsBar.Render("Read the 16-char code from the worker's screen (dashes OK).") + "\n\n")
	popup.WriteString(m.styles.Normal.Render("Code: ") + m.pairInput.View() + "\n")
	if m.statusMsg != "" {
		style := m.styles.Success
		if m.statusErr {
			style = m.styles.Error
		}
		popup.WriteString("\n" + style.Render(m.statusMsg) + "\n")
	}
	// Hint swaps to "continue" once the input is blurred (post-submit
	// result state) so the footer reflects the live action, not the
	// initial prompt.
	hint := "Enter confirm   Esc cancel"
	if !m.pairInput.Focused() {
		hint = "Enter continue"
	}
	popup.WriteString("\n" + m.styles.HintsBar.Render(hint))

	bg := m.viewList(width)
	return form.RenderOverlay(popup.String(), bg, width, height, m.styles)
}

// Breadcrumb implements tui.Breadcrumber.
func (m *NodesViewModel) Breadcrumb() []string {
	switch m.mode {
	case nodesViewDetail:
		if m.detailNode != nil {
			return []string{"Nodes", m.detailNode.Name}
		}
		c := m.list.Cursor()
		if c >= 0 && c < len(m.nodes) {
			return []string{"Nodes", m.nodes[c].Name}
		}
		return []string{"Nodes", "Details"}
	case nodesViewPair:
		return []string{"Nodes", "Pair Worker"}
	}
	return []string{"Nodes"}
}

func (m *NodesViewModel) navigateToBreadcrumb(level int) {
	if level == 1 {
		m.mode = nodesViewList
		m.confirmPending = false
		m.pairInput.Blur()
	}
}

func nodeRemoveAddress(n shared.NodeInfo) string {
	addr := strings.TrimSpace(n.Address)
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	if addr != "" {
		return addr
	}
	return strings.TrimSpace(n.IPAddress)
}

func (m *NodesViewModel) viewList(width int) string {
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.nodes) == 0,
		EmptyText: "No nodes found",
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
	}); done {
		return out
	}

	var b strings.Builder

	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
	// Keep the list's visible capacity in sync with the frame budget.
	// WindowSizeMsg doesn't always land on a freshly-mounted nested
	// view, which leaves l.visible at 0 → VisibleRange returns (0,0)
	// → zero rows even when items are populated. Re-applying here is
	// cheap and immunizes against the missed-window-size race.
	m.list.SetVisible(vis)
	start, end := m.list.VisibleRange()

	allNodeStatuses := make([]string, len(m.nodes))
	allRowStrs := make([][]string, len(m.nodes))
	for i, node := range m.nodes {
		allNodeStatuses[i] = shared.NodeDisplayStatus(node.HealthStatus)
		allRowStrs[i] = []string{node.Name, node.IPAddress, node.ClusterRole, allNodeStatuses[i], node.Disk, node.Memory, node.GPU}
	}

	visibleNodes := m.nodes[start:end]
	visibleStatuses := allNodeStatuses[start:end]

	rowStyles := make([]lipgloss.Style, len(visibleNodes))
	for i, s := range visibleStatuses {
		rowStyles[i] = shared.StatusStyle(m.styles, s)
	}

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"NODE", "IP", "ROLE", "STATUS", "STORAGE", "RAM", "VRAM"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: 3,
		RowStyles: rowStyles,
		AllRows:   allRowStrs,
	})

	for i, node := range visibleNodes {
		t.Row(node.Name, node.IPAddress, node.ClusterRole, visibleStatuses[i], node.Disk, node.Memory, node.GPU)
	}

	b.WriteString(t.Render() + "\n")

	if m.confirmPending {
		b.WriteString("\n" + m.styles.Error.Render(fmt.Sprintf("  Remove node %s? (y/n)", m.confirmTarget)) + "\n")
	} else if m.statusMsg != "" && m.mode != nodesViewPair {
		// In pair mode the popup owns the status message; don't echo
		// it into the list view underneath or it'll leak into the
		// background when the overlay renders.
		if m.statusErr {
			b.WriteString("  " + m.styles.Error.Render(m.statusMsg) + "\n")
		} else {
			b.WriteString("  " + m.styles.Success.Render(m.statusMsg) + "\n")
		}
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, shared.TableStatus("Nodes", len(m.nodes), m.list.Offset(), vis), nodesListHints()))

	return b.String()
}

func (m *NodesViewModel) viewDetail(width int) string { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.detailLoading {
		var b strings.Builder
		b.WriteString(" Loading...\n")
		b.WriteString("\n" + shared.RenderViewFooter(m.styles, width, "Esc back"))
		return b.String()
	}

	var node shared.NodeInfo
	if m.detailNode != nil {
		node = *m.detailNode
	} else {
		c := m.list.Cursor()
		if c >= 0 && c < len(m.nodes) {
			node = m.nodes[c]
		} else {
			return " No node selected\n"
		}
	}

	d := shared.NewDetail(m.styles, width)
	d.Field("Name", node.Name)
	d.Field("Address", node.Address)
	d.Field("Role", node.ClusterRole)
	d.Field("Status", node.HealthStatus)
	if node.LastError != "" {
		d.Text(" ", m.styles.Error.Render("Error: "+node.LastError))
	}
	d.Field("OS", node.OS)
	d.Field("Version", node.Version)
	if node.UptimeSeconds > 0 {
		d.Field("Uptime", shared.FormatUptimeDuration(node.UptimeSeconds))
	}

	if node.Disk != "" && node.Disk != "-" || node.Memory != "" && node.Memory != "-" {
		d.Section("Resources")
		if node.Disk != "" && node.Disk != "-" {
			d.Field("Storage", node.Disk)
		}
		if node.Memory != "" && node.Memory != "-" {
			d.Field("RAM", node.Memory)
		}
		if len(node.GPUs) > 0 {
			for _, gpu := range node.GPUs {
				vram := formatVRAM(gpu.VRAMFreeGB, gpu.VRAMTotalGB)
				d.Field(fmt.Sprintf("GPU %d", gpu.Index), fmt.Sprintf("%s (%s)", gpu.Name, vram))
				// Surface the per-card identifiers when the worker
				// reported them. Operators reading the detail view
				// to debug routing or pin a workload need PCI/UUID/
				// driver_index — without these, "GPU 0" is just a
				// label, not an addressable target.
				if gpu.PCIAddress != "" {
					d.Field("  PCI", gpu.PCIAddress)
				}
				if gpu.UUID != "" {
					d.Field("  UUID", gpu.UUID)
				}
				if gpu.DriverIndex != nil {
					d.Field("  Driver Index", fmt.Sprintf("%d (CUDA_VISIBLE_DEVICES / HIP_VISIBLE_DEVICES)", *gpu.DriverIndex))
				}
			}
		} else if node.GPUDesc != "" && node.GPUDesc != "-" {
			d.Field("GPU", node.GPUDesc)
		}
	}

	var localProviders, cloudProviders []shared.NodeProviderInfo
	for _, prov := range node.Providers {
		if prov.Cloud {
			cloudProviders = append(cloudProviders, prov)
		} else {
			localProviders = append(localProviders, prov)
		}
	}

	if len(localProviders) > 0 {
		d.Section("Local Providers")
		for _, prov := range localProviders {
			name := prov.Name
			if name == "" {
				name = prov.Key
			}
			val := prov.Type
			if prov.Formats != "" {
				val += " (" + prov.Formats + ")"
			}
			d.Field(name, val)
		}
	}

	if len(cloudProviders) > 0 && node.ClusterRole == string(config.ClusterModeCoordinator) {
		d.Section("Cloud Providers")
		for _, prov := range cloudProviders {
			name := prov.Name
			if name == "" {
				name = prov.Key
			}
			d.Field(name, prov.Type)
		}
	}

	gpuType := inferGPUType(node)
	if gpuType != "" {
		recs := recommendProviders(gpuType)
		if len(recs) > 0 {
			d.Section("Recommended Providers")
			for i, rec := range recs {
				prefix := "  "
				if i == 0 {
					prefix = m.styles.Success.Render("★") + " "
				}
				d.Write(fmt.Sprintf(" %s%-14s %s\n", prefix, m.styles.Normal.Render(rec.name), m.styles.Help.Render(rec.desc)))
			}
		}
	}

	allLines := strings.Split(strings.TrimRight(d.String(), "\n"), "\n")
	footer := shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		tui.Hint(tui.ListKeys.Up, "scroll"),
		tui.Hint(tui.ListKeys.PageUp, "page"),
		tui.Hint(tui.ListKeys.Back, "back"),
	))
	result, _ := shared.RenderDetailView(m.styles, allLines, m.detail.Scroll(), m.termHeight, footer, "", 0)
	return result
}

func inferGPUType(node shared.NodeInfo) string {
	var desc strings.Builder
	desc.WriteString(strings.ToLower(node.GPUDesc + " " + node.GPU))
	for _, gpu := range node.GPUs {
		desc.WriteString(" " + strings.ToLower(gpu.Name))
	}
	switch {
	case strings.Contains(desc.String(), "apple") || strings.Contains(desc.String(), "m1") ||
		strings.Contains(desc.String(), "m2") || strings.Contains(desc.String(), "m3") ||
		strings.Contains(desc.String(), "m4") || strings.Contains(desc.String(), "m5") ||
		strings.Contains(desc.String(), "metal"):
		return "apple"
	case strings.Contains(desc.String(), "nvidia") || strings.Contains(desc.String(), "cuda") ||
		strings.Contains(desc.String(), "rtx") || strings.Contains(desc.String(), "geforce") ||
		strings.Contains(desc.String(), "tesla") || strings.Contains(desc.String(), "a100") ||
		strings.Contains(desc.String(), "h100"):
		return "nvidia"
	case strings.Contains(desc.String(), "amd") || strings.Contains(desc.String(), "radeon") ||
		strings.Contains(desc.String(), "rocm"):
		return "amd"
	}
	if strings.Contains(strings.ToLower(node.OS), "darwin") || strings.Contains(strings.ToLower(node.OS), "macos") {
		return "apple"
	}
	return ""
}

type providerRec struct {
	key  string
	name string
	desc string
}

func recommendProviders(gpuType string) []providerRec {
	switch gpuType {
	case "apple":
		return []providerRec{
			{key: "mlx", name: "MLX", desc: "Native Apple Silicon, best performance"},
			{key: "ollama", name: "Ollama", desc: "Easy to use, large model library"},
			{key: "llamacpp", name: "llama.cpp", desc: "Lightweight, CPU/Metal hybrid"},
		}
	case "nvidia":
		return []providerRec{
			{key: "vllm", name: "vLLM", desc: "Native CUDA, highest throughput"},
			{key: "ollama", name: "Ollama", desc: "Easy to use, large model library"},
			{key: "llamacpp", name: "llama.cpp", desc: "Lightweight, CPU/CUDA hybrid"},
		}
	case "amd":
		return []providerRec{
			{key: "ollama", name: "Ollama", desc: "ROCm support, large model library"},
			{key: "llamacpp", name: "llama.cpp", desc: "Lightweight, CPU/ROCm hybrid"},
		}
	default:
		return []providerRec{
			{key: "ollama", name: "Ollama", desc: "Easy to use, large model library"},
			{key: "llamacpp", name: "llama.cpp", desc: "Lightweight, CPU inference"},
		}
	}
}

func fetchNodeDetail(client *pkgClient.Client, name string) nodeDetailMsg {
	resp, err := client.GetNode(name)
	if err != nil {
		return nodeDetailMsg{err: err}
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

	return nodeDetailMsg{node: node}
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
