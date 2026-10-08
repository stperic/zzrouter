package search

import (
	"fmt"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// View implements tea.Model for standalone use.
func (m SearchTUIModel) View() tea.View {
	titleBar := shared.RenderTitleBar(m.styles, m.TermWidth)
	v := shared.NewAltScreenView(titleBar+"\n"+m.ViewContent(), m.styles.Theme)
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// selectedModelName returns the selected model's name, or "model" as a fallback.
func (m SearchTUIModel) selectedModelName() string {
	if m.selectedItem != nil {
		return m.selectedItem.Name
	}
	return "model"
}

// breadcrumb returns navigation segments for the breadcrumb bar.
func (m SearchTUIModel) Breadcrumb() []string {
	cfg := m.getProviderConfig(m.Provider)
	prov := cfg.DisplayName

	switch m.State {
	case StateDetail, statePulling:
		return []string{"Model Hub", prov, m.selectedModelName()}
	case stateVariantSelect:
		return []string{"Model Hub", prov, m.selectedModelName(), "Quantization"}
	case stateProviderSelect:
		return []string{"Model Hub", "Select Provider"}
	case stateFilterEdit:
		return []string{"Model Hub", prov, "Filter"}
	case stateNodeSelect:
		return []string{"Model Hub", prov, m.selectedModelName(), "Select Node"}
	case stateDirectPull:
		return []string{"Model Hub", prov, "Direct Deploy"}
	default:
		return []string{"Model Hub", prov}
	}
}

// viewContent returns the view body without the title bar.
// The parent TUI renders the shared title bar and breadcrumb; this just returns the child content.
func (m SearchTUIModel) ViewContent() string {
	content := m.viewContentInner()
	if m.errorPopup != nil {
		// Overlay covers the child content area. termHeight is already the
		// child area in embedded mode; in standalone it excludes the title
		// bar, which is fine — we overlay only the child content.
		h := m.TermHeight
		if h <= 0 {
			h = 24
		}
		content = ui.RenderAndOverlayPopup(m.styles, content, m.TermWidth, h, *m.errorPopup)
	}
	return content
}

func (m SearchTUIModel) viewContentInner() string {
	switch m.State {
	case StateLoading:
		return m.viewLoading()
	case StateList:
		return m.viewList()
	case stateVariantSelect:
		return m.viewVariantSelect()
	case StateDetail:
		return m.viewDetail()
	case stateProviderSelect:
		return m.viewProviderSelect()
	case statePulling:
		return m.viewDeploying()
	case stateFilterEdit:
		return m.viewFilterEdit()
	case stateNodeSelect:
		return m.viewNodeSelect()
	case statePickerOverlay:
		return m.viewPickerOverlay()
	case stateTagAdd:
		return m.viewTagAdd()
	case stateDirectPull:
		return m.viewDirectDeploy()
	}
	return ""
}

// prerenderDescription renders the model description with glamour once (cached).
func (m SearchTUIModel) prerenderDescription() string {
	if m.selectedItem == nil || m.selectedItem.Description == "" {
		return ""
	}

	descContent := m.selectedItem.Description
	if len(descContent) > 10000 {
		descContent = descContent[:10000] + "\n\n..."
	}

	wrapWidth := max(m.TermWidth-4, 40)

	mdStyle := styles.DarkStyleConfig
	noMargin := uint(0)
	mdStyle.Document.Margin = &noMargin
	mdStyle.Document.BlockPrefix = ""
	mdStyle.Document.BlockSuffix = ""
	mdStyle.CodeBlock.Margin = &noMargin
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(mdStyle),
		glamour.WithWordWrap(wrapWidth),
		glamour.WithEmoji(),
	)
	var rendered string
	if err == nil {
		rendered, err = renderer.Render(descContent)
	}
	if err != nil || strings.TrimSpace(rendered) == "" {
		// Fallback to plain text
		desc := renderHTMLForTerminal(descContent)
		if len(desc) > 10000 {
			desc = desc[:10000] + "..."
		}
		return desc
	}
	return strings.TrimRight(rendered, "\n")
}

// viewLoading renders loading state
func (m SearchTUIModel) viewLoading() string {
	return m.styles.Title.Render(fmt.Sprintf("Searching for '%s'...", m.query)) + "\n"
}

// viewList renders the results list
func (m SearchTUIModel) viewList() string { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	var b strings.Builder
	cfg := m.getProviderConfig(m.Provider)

	// Error state — show error but keep footer so user can use Ctrl+D for direct deploy
	if m.err != nil {
		b.WriteString(" " + m.styles.Error.Render("✗ "+m.err.Error()) + "\n")
		b.WriteString(" " + m.styles.Help.Render("Use ^D to deploy a model by name directly.") + "\n")
		return m.padToBottom(b.String(), m.renderFooter(""))
	}

	// Empty results — show message but keep TUI alive
	if len(m.results) == 0 {
		b.WriteString(m.styles.Help.Render(" No models found matching your search.") + "\n")
		return m.padToBottom(b.String(), m.renderFooter(""))
	}

	// Determine metric column header based on sort
	metricHeader := "DOWNLOADS"
	switch m.sortBy {
	case "likes":
		metricHeader = "LIKES"
	case "createdAt", "created":
		metricHeader = "CREATED"
	}

	// Column layout per provider (driven by registry, not hardcoded names)
	isOllama := m.Provider == constants.RepoOllama
	isHuggingFace := m.Provider == constants.RepoHuggingFace || m.Provider == ""
	isCloud := cfg.IsCloud

	vis := m.listPageSize()

	tableWidth := max(m.TermWidth, 60)

	var headers []string
	if isCloud {
		headers = []string{"MODEL NAME", "CONTEXT", "PRICE", "ADDED"}
	} else if isHuggingFace {
		headers = []string{"MODEL NAME", "SIZE", "TREND", "DOWNLOADS", "LIKES", "UPDATED"}
	} else if isOllama {
		headers = []string{"MODEL NAME", "SIZE", "DOWNLOADS", "UPDATED"}
	} else {
		headers = []string{"MODEL NAME", metricHeader}
	}

	// Pre-build all rows for stable column sizing across pages
	allRowStrs := make([][]string, len(m.results))
	for i, result := range m.results {
		modelName := result.Name
		if result.Provider == constants.RepoHuggingFace && strings.Contains(result.Name, "/") {
			parts := strings.Split(result.Name, "/")
			if len(parts) > 1 {
				modelName = parts[len(parts)-1]
			}
		}
		if result.VariantCount > 0 {
			modelName += fmt.Sprintf(" [%d]", result.VariantCount)
		}

		metricDisplay := shared.FormatSearchDownloads(result.Downloads)
		switch m.sortBy {
		case "likes":
			metricDisplay = shared.FormatSearchDownloads(result.Likes)
		case "createdAt", "created":
			metricDisplay = result.CreatedAt
			if len(metricDisplay) > 10 {
				metricDisplay = metricDisplay[:10]
			}
		}

		if isCloud {
			ctxDisplay := result.ContextWindow
			if ctxDisplay == "" {
				ctxDisplay = shared.EmptyValue
			}
			priceDisp := shared.FormatCloudPrice(result.PricePrompt, result.PriceComplete)
			addedDisplay := shared.EmptyValue
			if result.CreatedAt != "" {
				addedDisplay = FormatRelativeTimeFromISO(result.CreatedAt)
			}
			allRowStrs[i] = []string{modelName, ctxDisplay, priceDisp, addedDisplay}
		} else if isHuggingFace {
			sizeDisplay := result.SizeDisplay
			if sizeDisplay == "" {
				sizeDisplay = "*"
			}
			trendDisplay := shared.EmptyValue
			if result.TrendingScore > 0 {
				trendDisplay = fmt.Sprintf("%.0f", result.TrendingScore)
			}
			downloadsDisplay := shared.FormatSearchDownloads(result.Downloads)
			likesDisplay := shared.FormatSearchDownloads(result.Likes)
			updatedDisplay := result.ModifiedAt
			if updatedDisplay == "" {
				updatedDisplay = result.CreatedAt
			}
			if result.Provider == constants.RepoHuggingFace {
				updatedDisplay = FormatRelativeTimeFromISO(updatedDisplay)
			}
			if updatedDisplay == "" {
				updatedDisplay = shared.EmptyValue
			}
			if result.Provider != constants.RepoHuggingFace {
				likesDisplay = shared.EmptyValue
			}
			allRowStrs[i] = []string{modelName, sizeDisplay, trendDisplay, downloadsDisplay, likesDisplay, updatedDisplay}
		} else if isOllama {
			sizeDisplay := result.SizeDisplay
			if sizeDisplay == "" {
				sizeDisplay = "*"
			}
			downloadsDisplay := shared.FormatSearchDownloads(result.Downloads)
			updatedDisplay := result.ModifiedAt
			if updatedDisplay == "" {
				updatedDisplay = shared.EmptyValue
			}
			allRowStrs[i] = []string{modelName, sizeDisplay, downloadsDisplay, updatedDisplay}
		} else {
			allRowStrs[i] = []string{modelName, metricDisplay}
		}
	}

	start, end := shared.TableSliceBounds(m.listOffset, vis, len(m.results))

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   headers,
		Width:     tableWidth,
		Vis:       vis,
		Cursor:    m.cursor,
		Offset:    m.listOffset,
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   allRowStrs,
	})

	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}

	b.WriteString(t.Render() + "\n")

	visEnd := end
	pageInfo := fmt.Sprintf("Showing %d-%d of %d", start+1, visEnd, len(m.results))

	sortName := m.sortBy
	if sortName == "" {
		sortName = cfg.defaultSort()
	}
	pageInfo += " | Sort: " + sortName

	if cfg.HasTags {
		if len(m.tagFilter) > 0 {
			pageInfo += " | Tags: " + strings.Join(m.tagFilter, ",")
		} else {
			pageInfo += " | Tags: all"
		}
	}

	// Show size legend if any sizes are missing
	if isHuggingFace || isOllama {
		hasMissing := false
		for _, r := range m.results {
			if r.SizeDisplay == "" {
				hasMissing = true
				break
			}
		}
		if hasMissing {
			pageInfo += " | * click model to see size"
		}
	}

	return m.padToBottom(b.String(), m.renderFooter(pageInfo))
}

// renderFooter builds the fixed footer:
// ─── separator ───
// Search (100)  Page 1/5 | Showing 1-22 of 100 results
// ─── separator ───
// 🔍 Type to filter ...
// ─── separator ───
// ↑/↓ navigate  PgUp/Dn page  Enter details ...
func (m SearchTUIModel) renderFooter(pageInfo string) string {
	var f strings.Builder
	w := m.TermWidth
	if w <= 0 {
		w = 80
	}
	sep := shared.Hrule(m.styles, w) + "\n"

	// Separator + status info
	f.WriteString(sep)
	left := " Search"
	if len(m.results) > 0 && pageInfo != "" {
		left = " " + pageInfo
	}
	f.WriteString(m.styles.HintsBar.Render(left) + "\n")

	// Separator + filter bar with blinking cursor
	f.WriteString(sep)
	cur := m.filterCursor.View()
	if m.inlineFilter == "" {
		f.WriteString(fmt.Sprintf(" %s%s\n", cur, m.styles.Help.Render("Type to filter (use ~prefix for broad search)...")))
	} else {
		f.WriteString(fmt.Sprintf(" %s%s\n", m.inlineFilter, cur))
	}

	// Shared footer: separator + hints (hide tags for providers that don't support them)
	hints := tui.SearchKeys.ListHintsString()
	if !m.getProviderConfig(m.Provider).HasTags {
		hints = tui.SearchKeys.ListHintsStringNoTags()
	}
	f.WriteString(shared.RenderViewFooter(m.styles, w, hints))
	f.WriteString("\n")

	return f.String()
}

// padToBottom takes content and footer strings and pads the content area
// so the footer is always pinned to the bottom of the terminal.
func (m SearchTUIModel) padToBottom(content, footer string) string {
	if m.TermHeight <= 0 {
		return content + footer
	}

	contentLines := strings.Count(content, "\n")
	footerLines := strings.Count(footer, "\n")
	totalLines := contentLines + footerLines

	// termHeight rows need termHeight-1 newlines (H lines separated by H-1 newlines).
	target := m.TermHeight - 1
	if totalLines < target {
		padding := target - totalLines
		content += strings.Repeat("\n", padding)
	}

	return content + footer
}

// viewDetail renders the model detail view with scrolling support
func (m SearchTUIModel) viewDetail() string { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	if m.selectedItem == nil {
		return ""
	}

	cfg := m.getProviderConfig(m.selectedItem.Provider)
	isCloud := cfg.IsCloud

	// Build content lines
	var lines []string
	// Values wider than the remaining space wrap into the value column
	// rather than running off the right edge.
	addField := func(key, value string) {
		rendered := m.styles.DetailKey.Render(key + ":")
		col := 1 + lipgloss.Width(rendered) + 1
		wrapped := strings.Split(shared.WrapText(value, m.TermWidth-col), "\n")
		lines = append(lines, fmt.Sprintf(" %s %s", rendered, m.styles.DetailValue.Render(wrapped[0])))
		for _, w := range wrapped[1:] {
			lines = append(lines, strings.Repeat(" ", col)+m.styles.DetailValue.Render(w))
		}
	}

	addField("Model", m.selectedItem.Name)

	if m.selectedVariant != nil {
		if m.selectedItem.Provider == constants.RepoOllama {
			addField("Variant", m.selectedVariant.Filename)
		} else {
			addField("Variant", fmt.Sprintf("%s (%s)", m.selectedVariant.QuantMethod, m.selectedVariant.SizeDisplay))
		}
	}

	addField("Provider", m.selectedItem.Provider)

	if isCloud {
		if m.selectedItem.ContextWindow != "" {
			addField("Context", m.selectedItem.ContextWindow)
		}
		if m.selectedItem.PriceDisplay != "" {
			addField("Price (in)", m.selectedItem.PriceDisplay)
		}
		if m.selectedItem.PriceComplete > 0 {
			outPrice := m.selectedItem.PriceComplete * 1_000_000
			if outPrice >= 1.0 {
				addField("Price (out)", fmt.Sprintf("$%.1f/M", outPrice))
			} else {
				addField("Price (out)", fmt.Sprintf("$%.2f/M", outPrice))
			}
		}
		if m.selectedItem.CreatedAt != "" {
			addField("Added", FormatRelativeTimeFromISO(m.selectedItem.CreatedAt))
		}
	} else {
		addField("Downloads", shared.FormatSearchDownloads(m.selectedItem.Downloads))
		if m.selectedItem.ModifiedAt != "" {
			addField("Updated", m.selectedItem.ModifiedAt)
		}
		sizeToShow := m.selectedItem.SizeDisplay
		if m.selectedVariant != nil && m.selectedVariant.SizeDisplay != "" && m.selectedVariant.SizeDisplay != shared.EmptyValue {
			sizeToShow = m.selectedVariant.SizeDisplay
		}
		if sizeToShow != "" && sizeToShow != shared.EmptyValue {
			addField("Size", sizeToShow)
		}
		if m.selectedItem.ContextWindow != "" {
			addField("Context", m.selectedItem.ContextWindow)
		}
	}

	// Build metadata lazily on first render
	if m.selectedItem.Metadata == nil && m.selectedItem.rawData != nil {
		m.selectedItem.Metadata = buildMetadata(m.selectedItem.rawData)
	}
	for _, kv := range m.selectedItem.Metadata {
		addField(kv.Key, kv.Value)
	}

	if m.selectedItem.Description != "" {
		lines = append(lines, fmt.Sprintf(" %s", m.styles.DetailKey.Render("Description:")))

		desc := m.renderedDesc
		if desc == "" {
			// Fallback if cache was not populated
			desc = m.prerenderDescription()
		}

		if desc != "" {
			for dl := range strings.SplitSeq(desc, "\n") {
				lines = append(lines, " "+dl)
			}
		}
	}

	if !isCloud && len(m.selectedItem.Tags) > 0 {
		tags := strings.Join(m.selectedItem.Tags[:min(5, len(m.selectedItem.Tags))], ", ")
		addField("Tags", tags)
	}

	if url := m.selectedItem.URL; url != "" {
		addField("URL", url)
	}

	// Shimmer prompt goes in scrollable content; deploy status goes in the fixed bar
	if m.PullStatus == "" {
		lines = append(lines, "")
		prompt := "Press P to deploy this model"
		if isCloud {
			prompt = "Press P to select this model"
		}
		lines = append(lines, " "+ui.Shimmer(prompt, m.shimmerFrame, "#d4a574"))
	}

	// Build status line for the fixed bar (shown instead of scroll arrows)
	var statusLine string
	if m.PullStatus != "" {
		if strings.HasPrefix(m.PullStatus, "✓") {
			statusLine = " " + m.styles.Success.Render(m.PullStatus)
		} else {
			statusLine = " " + m.styles.Error.Render(m.PullStatus)
		}
	}

	// Build footer and render with scroll
	footer := shared.RenderViewFooter(m.styles, m.TermWidth, tui.SearchKeys.DetailHintsString()) + "\n"
	result, _ := shared.RenderDetailView(m.styles, lines, m.detailScroll, m.TermHeight, footer, statusLine, 0)
	return result
}

// viewVariantSelect renders variant selection view
func (m SearchTUIModel) viewVariantSelect() string {
	var b strings.Builder

	isOllama := m.selectedItem != nil && m.selectedItem.Provider == constants.RepoOllama

	vis := m.variantPageSize()

	vtWidth := max(m.TermWidth, 60)

	var vtHeaders []string
	if isOllama {
		vtHeaders = []string{"NAME", "SIZE", "CONTEXT", "INPUT"}
	} else {
		vtHeaders = []string{"QUANT", "SIZE", "DESCRIPTION"}
	}

	vtStart, vtEnd := shared.TableSliceBounds(m.variantOffset, vis, len(m.variants))

	vt := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   vtHeaders,
		Width:     vtWidth,
		Vis:       vis,
		Cursor:    m.variantCursor,
		Offset:    m.variantOffset,
		Styles:    m.styles,
		StatusCol: -1,
	})

	for _, variant := range m.variants[vtStart:vtEnd] {
		if isOllama {
			vt.Row(variant.Filename, variant.SizeDisplay, variant.QuantMethod, variant.Description)
		} else {
			vt.Row(variant.QuantMethod, variant.SizeDisplay, variant.Description)
		}
	}

	b.WriteString(vt.Render() + "\n")

	vsort := m.variantSort
	if vsort == "" {
		vsort = "recommended"
	}
	status := shared.TableStatus("Variants", len(m.variants), m.variantOffset, vis) + " | Sort: " + vsort
	hints := tui.JoinHints(
		tui.Hint(tui.SearchKeys.Up, "navigate"),
		tui.Hint(tui.SearchKeys.PageUp, "page"),
		tui.Hint(tui.SearchSubKeys.Enter, "details"),
		tui.Hint(tui.SearchKeys.Sort, "sort"),
		tui.Hint(tui.SearchSubKeys.Back, "back"),
	)

	footer := shared.RenderViewFooterWithStatus(m.styles, m.TermWidth, status, hints) + "\n"
	return m.padToBottom(b.String(), footer)
}

// viewProviderSelect renders provider selection view
func (m SearchTUIModel) viewProviderSelect() string {
	var b strings.Builder

	modelName := "model"
	if m.selectedItem != nil {
		modelName = m.selectedItem.Name
	}
	b.WriteString(" " + m.styles.Normal.Render(fmt.Sprintf("Choose where to download '%s':", modelName)) + "\n\n")

	for i, opt := range m.providerOptions {
		line := fmt.Sprintf("%d. %s", i+1, opt.Display)
		if i == m.providerCursor {
			b.WriteString(" " + m.styles.Selected.Render(line) + "\n")
		} else {
			b.WriteString(" " + m.styles.Normal.Render(line) + "\n")
		}
	}

	b.WriteString("\n" + shared.RenderViewFooter(m.styles, m.TermWidth, tui.JoinHints(
		tui.Hint(tui.SearchSubKeys.Up, "navigate"),
		tui.Hint(tui.SearchSubKeys.Enter, "select"),
		tui.Hint(tui.SearchSubKeys.Back, "back"),
	)))
	b.WriteString("\n")

	return b.String()
}

// viewDeploying renders pulling state
func (m SearchTUIModel) viewDeploying() string {
	modelName := "model"
	if m.selectedItem != nil {
		modelName = m.selectedItem.Name
	}
	return m.styles.Title.Render(fmt.Sprintf("⏳ Pulling model '%s'...", modelName)) + "\n"
}

// viewFilterEdit renders the filter edit overlay
func (m SearchTUIModel) viewFilterEdit() string {
	var b strings.Builder

	// Show input box
	b.WriteString(" " + m.styles.DetailKey.Render("Filter: ") + "\n")
	b.WriteString(" " + m.styles.Selected.Render(fmt.Sprintf(" %s█ ", m.filterInput)) + "\n\n")

	b.WriteString(shared.RenderViewFooter(m.styles, m.TermWidth, "Enter apply  Esc cancel"))
	b.WriteString("\n")

	return b.String()
}

// viewNodeSelect renders the multi-select node picker with resource info.
func (m SearchTUIModel) viewNodeSelect() string {
	var b strings.Builder

	// Model info line
	modelName := "model"
	details := ""
	if m.selectedItem != nil {
		modelName = m.selectedItem.Name
		var parts []string
		if m.selectedVariant != nil && m.selectedVariant.Filename != "" {
			parts = append(parts, m.selectedVariant.Filename)
		}
		if m.selectedVariant != nil && m.selectedVariant.SizeDisplay != "" && m.selectedVariant.SizeDisplay != shared.EmptyValue {
			parts = append(parts, m.selectedVariant.SizeDisplay)
		} else if m.selectedItem.SizeDisplay != "" {
			parts = append(parts, m.selectedItem.SizeDisplay)
		}
		if len(m.selectedItem.Tags) > 0 {
			tags := strings.Join(m.selectedItem.Tags[:min(3, len(m.selectedItem.Tags))], ", ")
			parts = append(parts, tags)
		}
		if len(parts) > 0 {
			details = " (" + strings.Join(parts, " | ") + ")"
		}
	}
	b.WriteString(m.styles.Title.Render(fmt.Sprintf("Deploy Model: %s%s", modelName, details)) + "\n\n")

	if len(m.availableNodes) == 0 {
		b.WriteString(" " + m.styles.Error.Render("No nodes available for download") + "\n")
		b.WriteString("\n" + shared.RenderViewFooter(m.styles, m.TermWidth, "Esc back") + "\n")
		return b.String()
	}

	displayCursor := max(m.hostCursor, 0)
	if displayCursor >= len(m.availableNodes) {
		displayCursor = len(m.availableNodes) - 1
	}

	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"NODE", "ROLE", "STORAGE", "RAM", "VRAM"},
		ColWidths: nil,
		Width:     m.TermWidth,
		Vis:       len(m.availableNodes),
		Cursor:    displayCursor,
		Offset:    0,
		Styles:    m.styles,
		StatusCol: -1,
	})

	for _, host := range m.availableNodes {
		check := "[ ] "
		if m.selectedNodes[host.name] {
			check = "[x] "
		}

		disk := shared.EmptyValue
		if host.diskTotalGB > 0 {
			disk = fmt.Sprintf("%.0f / %.0f GB", host.diskAvailableGB, host.diskTotalGB)
		}

		ram := shared.EmptyValue
		if host.ramTotalGB > 0 {
			ram = fmt.Sprintf("%.0f / %.0f GB", host.ramAvailableGB, host.ramTotalGB)
		}

		role := host.role
		if role == "" {
			role = shared.EmptyValue
		}

		vram := shared.EmptyValue
		if host.vramTotalGB > 0 {
			vram = fmt.Sprintf("%.0f / %.0f GB [%d]", host.vramAvailableGB, host.vramTotalGB, host.gpuCount)
		}

		t.Row(check+host.name, role, disk, ram, vram)
	}

	b.WriteString(t.Render() + "\n")

	selected := 0
	for _, v := range m.selectedNodes {
		if v {
			selected++
		}
	}
	status := fmt.Sprintf("Compatible Nodes (%d)  %d selected", len(m.availableNodes), selected)
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, m.TermWidth, status, tui.JoinHints(
		tui.Hint(tui.SearchSubKeys.Up, "navigate"),
		tui.Hint(tui.SearchSubKeys.Space, "toggle"),
		tui.Hint(tui.SearchSubKeys.Enter, "deploy"),
		tui.Hint(tui.SearchSubKeys.Back, "back"),
	)))

	return b.String()
}

// renderPopupOverList renders a styled popup box centered over the list view.
// popupContent is the rendered content inside the box; popupWidth is the desired box width.
func (m SearchTUIModel) renderPopupOverList(popupContent string, popupWidth int) string {
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.Theme.Primary).
		BorderBackground(m.styles.Theme.Mantle).
		Background(m.styles.Theme.Mantle).
		Padding(1, 2).
		Width(popupWidth).
		Render(popupContent)
	return ui.OverlayPopup(m.styles, m.viewList(), box, m.TermWidth, m.TermHeight)
}

// viewPickerOverlay renders a centered popup over the dimmed list view.
func (m SearchTUIModel) viewPickerOverlay() string {
	// Build the popup content
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render(m.pickerTitle) + "\n\n")

	for i, item := range m.pickerItems {
		prefix := "  "
		if i == m.pickerCursor {
			prefix = "\u25b8 " // ▸
		}
		line := prefix + item
		if i == m.pickerCursor {
			popup.WriteString(m.styles.Selected.Render(line) + "\n")
		} else {
			popup.WriteString(m.styles.Normal.Render(line) + "\n")
		}
	}

	if m.pickerAction == "tags" {
		popup.WriteString("\n" + m.styles.Help.Render(tui.JoinHints(
			tui.Hint(tui.SearchSubKeys.Up, "navigate"),
			tui.Hint(tui.SearchSubKeys.Enter, "select"),
			"a add", "d delete",
			tui.Hint(tui.SearchSubKeys.Back, "cancel"),
		)))
	} else {
		popup.WriteString("\n" + m.styles.Help.Render(tui.JoinHints(
			tui.Hint(tui.SearchSubKeys.Up, "navigate"),
			tui.Hint(tui.SearchSubKeys.Enter, "select"),
			tui.Hint(tui.SearchSubKeys.Back, "cancel"),
		)))
	}

	// Calculate popup Width: widest line + padding, clamped to terminal
	maxLineWidth := lipgloss.Width(m.pickerTitle) + 4
	for _, item := range m.pickerItems {
		w := lipgloss.Width(item) + 4 // prefix + padding
		if w > maxLineWidth {
			maxLineWidth = w
		}
	}
	popupWidth := max(
		// border + generous inner padding
		maxLineWidth+16, 40)
	if popupWidth > m.TermWidth-4 && m.TermWidth > 10 {
		popupWidth = m.TermWidth - 4
	}

	return m.renderPopupOverList(popup.String(), popupWidth)
}

// viewTagAdd renders the add-tag text input popup over the list view.
func (m SearchTUIModel) viewTagAdd() string {
	var popup strings.Builder
	popup.WriteString(m.styles.Title.Render("Add Tag Filter") + "\n\n")
	popup.WriteString(" " + m.styles.DetailKey.Render("Tag:") + " " + m.tagAddInput + "█\n")
	popup.WriteString("\n" + m.styles.Help.Render(tui.JoinHints(
		tui.Hint(tui.SearchSubKeys.Enter, "add"),
		tui.Hint(tui.SearchSubKeys.Back, "cancel"),
	)))

	popupWidth := 40
	if m.TermWidth > 50 {
		popupWidth = 44
	}

	return m.renderPopupOverList(popup.String(), popupWidth)
}

// viewDirectDeploy renders the direct deploy overlay popup.
func (m SearchTUIModel) viewDirectDeploy() string {
	var popup strings.Builder

	cfg := m.getProviderConfig(m.Provider)
	popup.WriteString(m.styles.Title.Render("Direct Deploy: "+cfg.DisplayName) + "\n\n")

	// Provider-specific hint
	switch m.Provider {
	case constants.RepoOllama:
		popup.WriteString(m.styles.Help.Render("  Enter model name (e.g., llama3:8b, minimax-m2.7:cloud)") + "\n\n")
	case constants.RepoHuggingFace:
		popup.WriteString(m.styles.Help.Render("  Enter model name (e.g., bartowski/Qwen3-8B-GGUF)") + "\n\n")
	default:
		popup.WriteString(m.styles.Help.Render("  Enter model name") + "\n\n")
	}

	// Input field with cursor
	input := m.directPullInput
	cursorChar := m.styles.Accent.Render("█")
	popup.WriteString("  " + input + cursorChar + "\n\n")

	// Error or loading state
	if m.directPullLoading {
		popup.WriteString(m.styles.Help.Render("  Validating model...") + "\n\n")
	} else if m.directPullError != "" {
		popup.WriteString("  " + m.styles.Error.Render("✗ "+m.directPullError) + "\n\n")
	}

	// Footer hints
	popup.WriteString(m.styles.Help.Render("  Enter deploy  Esc cancel") + "\n")

	popupWidth := 60
	if m.TermWidth > 80 {
		popupWidth = 70
	}

	return m.renderPopupOverList(popup.String(), popupWidth)
}
