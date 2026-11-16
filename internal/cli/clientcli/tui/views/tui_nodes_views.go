package views

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// View implements tea.Model.
func (m *NodesViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.help.View(m.styles, m.viewContent(), m.termWidth, m.termHeight, nodesListHints()))
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
	popup.WriteString(m.styles.HintsBar.Render("Read the URL, fingerprint and code from the worker's screen.") + "\n\n")
	popup.WriteString(m.styles.Normal.Render("Worker URL: ") + m.pairURL.View() + "\n")
	popup.WriteString(m.styles.Normal.Render("Fingerprint: ") + m.pairFingerprint.View() + "\n")
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
	if !m.pairFormFocused() {
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
		Headers:        []string{"NODE", "IP", "ROLE", "STATUS", "STORAGE", "RAM", "VRAM"},
		ColumnPriority: []int{100, 20, 50, 90, 10, 70, 60},
		ColWidths:      []int{16, 15, 12, 9, 14, 16, 16},
		Width:          width,
		Vis:            vis,
		Cursor:         m.list.Cursor(),
		Offset:         m.list.Offset(),
		Styles:         m.styles,
		StatusCol:      3,
		RowStyles:      rowStyles,
		AllRows:        allRowStrs,
	})

	for i, node := range visibleNodes {
		t.Row(node.Name, node.IPAddress, node.ClusterRole, visibleStatuses[i], node.Disk, node.Memory, node.GPU)
	}

	b.WriteString(t.Render() + "\n")

	status := shared.TableStatus("Nodes", len(m.nodes), m.list.Offset(), vis)
	if m.statusMsg != "" && m.mode != nodesViewPair {
		status = m.statusMsg
	}
	hints := nodesListHints() + "  ? help"
	if m.confirmPending {
		status = "Remove " + m.confirmTarget + "?"
		hints = "Y remove  Enter/Esc cancel"
	}
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, hints))

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
