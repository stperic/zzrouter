package search

import (
	"fmt"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// pickerKeyNone is the sentinel key for "no filter" in the tag picker.
const pickerKeyNone = "__none__"

// rebuildTagPickerItems rebuilds pickerItems and pickerKeys from tagPresets.
// Used after adding or deleting a tag preset.
func (m *SearchTUIModel) rebuildTagPickerItems() {
	m.pickerItems = []string{"All (no filter)"}
	m.pickerKeys = []string{pickerKeyNone}
	for _, p := range m.tagPresets {
		m.pickerItems = append(m.pickerItems, strings.Join(p.Tags, ", "))
		m.pickerKeys = append(m.pickerKeys, strings.Join(p.Tags, ","))
	}
}

// Update handles messages
func (m SearchTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// In embedded mode the parent already set termHeight to the child area
		// before forwarding the message — don't overwrite it.
		// In standalone mode we subtract the title bar ourselves.
		m.TermWidth = msg.Width
		if !m.embedded {
			m.TermHeight = msg.Height - shared.TuiTitleBarLines
		}

		return m, nil

	case tea.MouseClickMsg:
		// Right-click = go back one level
		if msg.Button == tea.MouseRight {
			switch m.State {
			case StateDetail:
				if m.prevState == stateVariantSelect && len(m.variants) > 1 {
					m.State = stateVariantSelect
				} else {
					m.State = StateList
				}
				m.selectedItem = nil
				m.selectedVariant = nil
				m.renderedDesc = ""
				m.PullStatus = ""
			case stateVariantSelect:
				m.State = StateList
				m.variantCursor = 0
				m.variantOffset = 0
				m.selectedVariant = nil
			case stateNodeSelect:
				m.State = StateDetail
				m.PullStatus = ""
			case stateProviderSelect:
				m.State = StateDetail
			case statePickerOverlay:
				m.State = m.prevState
			case StateList:
				return m, m.quitOrBack()
			}
			return m, nil
		}

		switch m.State {
		case StateList:
			enter, ok := shared.TableClick(msg, &m.cursor, m.listOffset, len(m.results), shared.TuiTableClickY)
			if ok && enter && m.cursor < len(m.results) {
				m.selectedItem = &m.results[m.cursor]
				m.PullStatus = ""
				m.selectedVariant = nil
				m.detailScroll = 0
				return m, m.loadVariants()
			}
		case stateVariantSelect:
			enter, ok := shared.TableClick(msg, &m.variantCursor, m.variantOffset, len(m.variants), shared.TuiTableClickY)
			if ok && enter && m.variantCursor < len(m.variants) {
				m.selectedVariant = &m.variants[m.variantCursor]
				m.prevState = stateVariantSelect
				m.State = StateDetail
				return m, m.fetchModelDetails()
			}
		case stateNodeSelect:
			enter, ok := shared.TableClick(msg, &m.hostCursor, 0, len(m.availableNodes), shared.TuiTableClickY)
			if ok && enter {
				name := m.availableNodes[m.hostCursor].name
				m.selectedNodes[name] = !m.selectedNodes[name]
			}
		}
		return m, nil

	case tea.MouseWheelMsg:
		switch m.State {
		case StateList:
			shared.MouseWheel(msg, &m.cursor, &m.listOffset, len(m.results), m.listPageSize())
		case stateVariantSelect:
			shared.MouseWheel(msg, &m.variantCursor, &m.variantOffset, len(m.variants), m.variantPageSize())
		case stateNodeSelect:
			shared.MouseWheel(msg, &m.hostCursor, new(int), len(m.availableNodes), len(m.availableNodes))
		case StateDetail:
			shared.DetailMouseWheel(msg, &m.detailScroll)
		}
		return m, nil

	case tea.KeyPressMsg:
		// Modal error popup traps all keys — Esc/Enter/q dismiss, others no-op.
		if m.errorPopup != nil {
			switch msg.String() {
			case "esc", "enter", "q":
				m.errorPopup = nil
			}
			return m, nil
		}
		switch m.State {
		case StateList:
			return m.updateList(msg)
		case stateVariantSelect:
			return m.updateVariantSelect(msg)
		case StateDetail:
			return m.updateDetail(msg)
		case stateProviderSelect:
			return m.updateProviderSelect(msg)
		case statePulling:
			return m.updateDeploying(msg)
		case stateFilterEdit:
			return m.updateFilterEdit(msg)
		case stateNodeSelect:
			return m.updateNodeSelect(msg)
		case statePickerOverlay:
			return m.updatePickerOverlay(msg)
		case stateDirectPull:
			return m.updateDirectDeploy(msg)
		case stateTagAdd:
			return m.updateTagAdd(msg)
		}

	case debounceSearchMsg:
		// Only fire if no newer keystrokes happened since this was scheduled
		if msg.query == m.inlineFilter {
			m.broadSearch = isBroadSearch(m.inlineFilter)
			m.query = convertWildcard(m.inlineFilter)
			m.cursor = 0
			m.listOffset = 0

			cfg := m.getProviderConfig(m.Provider)

			// Providers with ClientFilter use cached results (no re-fetch)
			if cfg.ClientFilter && len(m.allResults) > 0 {
				m.results = filterResultsByName(m.allResults, m.inlineFilter)
				sortBy := m.sortBy
				if sortBy == "" {
					sortBy = cfg.defaultSort()
				}
				if cfg.ClientSorts[sortBy] {
					m.results = applyClientSort(m.results, sortBy)
				}
				return m, nil
			}
			// Server-side search
			return m, m.fetchResults()
		}
		return m, nil

	case searchCompleteMsg:
		m.State = StateList
		m.allResults = msg.results // Cache full results for client-side filtering
		m.results = msg.results
		m.err = msg.err
		if m.err != nil {
			m.State = StateList // Show error in list view
		}
		// Apply default client-side sort if needed (e.g., Ollama defaults to downloads)
		if m.err == nil && len(m.results) > 0 {
			cfg := m.getProviderConfig(m.Provider)
			sortBy := m.sortBy
			if sortBy == "" {
				sortBy = cfg.defaultSort()
			}
			if cfg.ClientSorts[sortBy] {
				m.results = applyClientSort(m.results, sortBy)
				m.allResults = applyClientSort(m.allResults, sortBy)
			}
		}

	case ui.ShimmerTickMsg:
		// Advance shimmer animation when in detail view without deploy status
		if m.State == StateDetail && m.PullStatus == "" {
			m.shimmerFrame++
			return m, ui.ShimmerTickCmd()
		}
		return m, nil

	case providerLoadedMsg:
		if msg.err != nil {
			m.State = StateDetail
			m.PullStatus = formatErrorMessage("Failed to load providers: %v", msg.err)
		} else {
			m.providerOptions = msg.options
			m.providerCursor = 0
			m.State = stateProviderSelect
		}

	case hostsLoadedMsg:
		if msg.err != nil {
			m.State = StateDetail
			// Compatibility failures get a modal popup with install guidance;
			// everything else stays in the status bar.
			if strings.Contains(msg.err.Error(), "no compatible provider") {
				m.errorPopup = buildIncompatPopup(msg.err.Error())
				m.PullStatus = ""
			} else {
				m.PullStatus = formatErrorMessage("%v", msg.err)
			}
		} else if len(msg.hosts) == 0 {
			m.State = StateDetail
			m.PullStatus = formatErrorMessage("No nodes available for download")
		} else if m.selectedItem == nil {
			// Safety check: ensure model is still selected
			m.State = StateDetail
			m.PullStatus = formatErrorMessage("No model selected")
		} else {
			m.availableNodes = msg.hosts
			m.hostCursor = 0
			m.selectedNodes = make(map[string]bool)
			for _, h := range msg.hosts {
				m.selectedNodes[h.name] = true
			}
			// Single-node cluster with a compatible .Provider: skip the picker
			// and deploy directly — less friction for new users. Multi-node
			// clusters always see the picker so users know where it landed.
			if m.NodeCount == 1 && len(msg.hosts) == 1 {
				m.State = statePulling
				return m, m.deployModelToNodes([]string{msg.hosts[0].name})
			}
			m.State = stateNodeSelect
		}

	case deployValidateMsg:
		m.directPullLoading = false
		if msg.err != nil {
			// Validation failed — show error in overlay (if still in overlay) or detail view
			if m.State == stateDirectPull {
				m.directPullError = msg.err.Error()
			} else {
				m.PullStatus = formatErrorMessage("Deploy failed: %v", msg.err)
			}
			return m, nil
		}

		// Validation succeeded — build synthetic item only for direct deploy
		// (when pressing P from detail view, selectedItem already has full details)
		if m.selectedItem == nil || (m.selectedItem.Name != msg.modelName && m.selectedItem.APIName() != msg.modelName) {
			m.selectedItem = &searchResult{
				Provider: msg.provider,
				Name:     msg.modelName,
			}
		}

		// Cloud variant or cloud provider → register directly
		if isOllamaCloudTag(msg.modelName) || m.isCloudVariantSelected() || m.getProviderConfig(msg.provider).IsCloud {
			if isOllamaCloudTag(msg.modelName) {
				m.selectedVariant = &ModelVariant{Filename: "cloud"}
			}
			m.PullStatus = "Registering cloud model..."
			m.State = StateDetail
			return m, m.deployModelToNodes([]string{""})
		}

		// Local model → node selection
		m.PullStatus = "Checking available nodes..."
		m.State = StateDetail
		m.directPullInput = ""
		m.directPullError = ""
		return m, m.loadAvailableNodes()

	case deployCompleteMsg:
		m.State = StateDetail
		if msg.success {
			m.PullStatus = fmt.Sprintf("%s Deploy started! Press m to go to Models view.", ui.GetCheckEmoji())
		} else {
			m.PullStatus = formatErrorMessage("Deploy failed: %v", msg.err)
		}

	case modelDetailsMsg:
		if msg.err == nil && m.selectedItem != nil {
			if msg.description != "" {
				m.selectedItem.Description = msg.description
			}
			m.selectedItem.Metadata = mergeMetadata(m.selectedItem.Metadata, msg.metadata)
			m.renderedDesc = m.prerenderDescription()
		}
		// Start shimmer animation for "Press P" prompt
		if m.PullStatus == "" {
			m.shimmerFrame = 0
			return m, ui.ShimmerTickCmd()
		}

	case variantsLoadedMsg:
		if msg.err != nil {
			// No variants or error - go directly to detail view
			m.prevState = StateList
			m.State = StateDetail
			return m, m.fetchModelDetails()
		} else if len(msg.variants) > 1 {
			// Multiple variants - show selection
			m.variants = msg.variants
			m.variantCursor = 0
			m.variantOffset = 0
			m.State = stateVariantSelect
			return m, nil
		} else if len(msg.variants) == 1 {
			// Single variant - auto-select and go to detail
			m.selectedVariant = &msg.variants[0]
			m.prevState = StateList
			m.State = StateDetail
			return m, m.fetchModelDetails()
		} else {
			// No variants - go to detail
			m.prevState = StateList
			m.State = StateDetail
			return m, m.fetchModelDetails()
		}

	case providerConfigsLoadedMsg:
		if len(msg.keys) > 0 && len(msg.configs) > 0 {
			m.providerKeys = msg.keys
			m.providerConfigs = msg.configs

			// If current provider is no longer enabled, reset to first available
			if m.Provider != "" {
				cfg := m.getProviderConfig(m.Provider)
				if !cfg.Enabled {
					// Find first enabled provider
					for _, k := range msg.keys {
						if c, ok := msg.configs[k]; ok && c.Enabled {
							m.Provider = k
							return m, m.fetchResults()
						}
					}
				}
			} else if len(msg.keys) > 0 {
				// No provider set — use first enabled
				for _, k := range msg.keys {
					if c, ok := msg.configs[k]; ok && c.Enabled {
						m.Provider = k
						return m, m.fetchResults()
					}
				}
			}
		}

	case cloudModelAddedMsg:
		m.State = StateDetail
		if msg.err != nil {
			m.PullStatus = formatErrorMessage("Failed to add .Model: %v", msg.err)
		} else {
			providerName := m.Provider
			if m.selectedItem != nil {
				providerName = m.getProviderConfig(m.selectedItem.Provider).DisplayName
			}
			m.PullStatus = fmt.Sprintf("%s Cloud model '%s' is available via the %s API. Use it in chat with: /model %s",
				ui.GetCheckEmoji(), msg.modelID, providerName, msg.modelID)
		}
	}

	// Forward to cursor for blink animation
	var cursorCmd tea.Cmd
	m.filterCursor, cursorCmd = m.filterCursor.Update(msg)
	return m, cursorCmd
}

// updateList handles list view key presses
// All printable characters go to the inline filter (fzf pattern).
// Shortcuts use Ctrl+ modifiers to avoid clashing with filter input.
func (m SearchTUIModel) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch {
	case key.Matches(msg, tui.SearchKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchKeys.Clear):
		if m.inlineFilter != "" {
			m.inlineFilter = ""
			m.query = ""
			m.State = StateLoading
			m.cursor = 0
			m.listOffset = 0
			return m, m.fetchResults()
		}
		if m.embedded {
			return m, m.quitOrBack()
		}

	// Direct deploy — always available, even on search error
	case key.Matches(msg, tui.SearchKeys.DirectPull):
		if m.Provider == constants.RepoOllama || m.Provider == constants.RepoHuggingFace {
			m.directPullInput = ""
			m.prevState = m.State
			m.State = stateDirectPull
			return m, nil
		}

	default:
		nav := shared.ListNav{Cursor: &m.cursor, Offset: &m.listOffset, Total: len(m.results), Vis: m.listPageSize()}
		if nav.Navigate(msg, tui.SearchKeys.Up, tui.SearchKeys.Down, tui.SearchKeys.PageUp, tui.SearchKeys.PageDown, tui.SearchKeys.Home, tui.SearchKeys.End) {
			return m, nil
		}

		switch {
		case key.Matches(msg, tui.SearchKeys.Provider):
			allKeys := m.providerKeys
			if len(allKeys) == 0 {
				allKeys = defaultProviderKeys
			}
			// Filter: show only enabled providers
			var keys []string
			var items []string
			for _, k := range allKeys {
				cfg := m.getProviderConfig(k)
				if !cfg.Enabled {
					continue // skip disabled providers
				}
				label := cfg.Icon + " " + cfg.DisplayName
				keys = append(keys, k)
				items = append(items, label)
			}
			m.pickerTitle = "Select a Model Hub Registry"
			m.pickerItems = items
			m.pickerKeys = keys
			m.pickerCursor = 0
			current := m.Provider
			if current == "" {
				current = constants.RepoHuggingFace
			}
			for i, k := range keys {
				if k == current {
					m.pickerCursor = i
					break
				}
			}
			m.pickerAction = "provider"
			m.prevState = m.State
			m.State = statePickerOverlay

		case key.Matches(msg, tui.SearchKeys.Tags):
			if !m.getProviderConfig(m.Provider).HasTags {
				break
			}

			items := []string{"All (no filter)"}
			keys := []string{pickerKeyNone}

			if m.Provider == constants.RepoOllama {
				// Ollama: built-in cloud/local filter (no HF tag presets)
				items = append(items, "Local only", "Cloud only")
				keys = append(keys, "local", "cloud")
			} else {
				// User-defined tag presets from config (HuggingFace, etc.)
				for _, p := range m.tagPresets {
					items = append(items, strings.Join(p.Tags, ", "))
					keys = append(keys, strings.Join(p.Tags, ","))
				}
			}

			if len(items) <= 1 {
				break // Only "All" — nothing to pick
			}

			m.pickerTitle = "Tag Filter"
			m.pickerItems = items
			m.pickerKeys = keys
			m.pickerCursor = max(m.tagPresetIndex+1, 0)
			m.pickerAction = "tags"
			m.prevState = m.State
			m.State = statePickerOverlay

		case key.Matches(msg, tui.SearchKeys.Sort):
			cfg := m.getProviderConfig(m.Provider)
			m.pickerTitle = "Sort by"
			m.pickerItems = cfg.SortOptions
			m.pickerKeys = cfg.SortOptions
			m.pickerCursor = 0
			for i, s := range cfg.SortOptions {
				if s == m.sortBy {
					m.pickerCursor = i
					break
				}
			}
			m.pickerAction = "sort"
			m.prevState = m.State
			m.State = statePickerOverlay

		case key.Matches(msg, tui.SearchKeys.Enter):
			if m.cursor < len(m.results) {
				m.selectedItem = &m.results[m.cursor]
				m.PullStatus = ""
				m.selectedVariant = nil
				m.detailScroll = 0
				return m, m.loadVariants()
			}

		case key.Matches(msg, tui.SearchKeys.Backspace):
			if len(m.inlineFilter) > 0 {
				m.inlineFilter = m.inlineFilter[:len(m.inlineFilter)-1]
				m.debounceSeqNo++
				return m, m.scheduleDebounceSearch()
			}

		default:
			// All printable characters go to the inline filter
			ch := msg.String()
			if len(ch) == 1 && ch[0] >= 32 && ch[0] < 127 {
				m.inlineFilter += ch
				m.debounceSeqNo++
				return m, m.scheduleDebounceSearch()
			}
		}
	}

	return m, nil
}

// scheduleDebounceSearch schedules a debounced search after 500ms
func (m SearchTUIModel) scheduleDebounceSearch() tea.Cmd {
	currentFilter := m.inlineFilter
	return tea.Tick(500*time.Millisecond, func(_ time.Time) tea.Msg {
		return debounceSearchMsg{query: currentFilter}
	})
}

// convertWildcard extracts the API search term from a wildcard pattern.
// For "qwen*mlx", sends "qwen mlx" to the API so HuggingFace narrows results
// server-side. The full wildcard matching is done client-side in matchesWildcard().
func convertWildcard(input string) string {
	// Strip ~ prefix (broad search indicator)
	cleaned := strings.TrimPrefix(strings.TrimSpace(input), "~")
	cleaned = strings.TrimSpace(cleaned)

	// Split on * and join all non-empty parts with spaces
	// HuggingFace search= parameter supports multiple words
	// "qwen*mlx" → "qwen mlx" → returns models matching both terms
	parts := strings.Split(cleaned, "*")
	var terms []string
	for _, p := range parts {
		p = strings.ReplaceAll(p, "?", "")
		p = strings.TrimSpace(p)
		if p != "" {
			terms = append(terms, p)
		}
	}
	return strings.Join(terms, " ")
}

// matchesWildcard checks if a model name matches a wildcard filter.
// Supports * (any characters) by requiring all non-empty parts between *
// to appear in the name in order.
// Examples: "qwen3*mlx" matches "Qwen3-0.6B-MLX-8bit"
func matchesWildcard(name, filter string) bool {
	// Strip ~ prefix
	filter = strings.TrimPrefix(strings.TrimSpace(filter), "~")
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}

	nameLower := strings.ToLower(name)
	filterLower := strings.ToLower(filter)

	// Split on * to get required parts
	parts := strings.Split(filterLower, "*")
	pos := 0
	for _, part := range parts {
		part = strings.ReplaceAll(part, "?", "") // ? treated as zero-width for now
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.Index(nameLower[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	return true
}

// isBroadSearch returns true if the inline filter starts with ~ (search name + description + tags)
func isBroadSearch(filter string) bool {
	return strings.HasPrefix(strings.TrimSpace(filter), "~")
}

// updateDetail handles detail view key presses
func (m SearchTUIModel) updateDetail(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		if m.prevState == stateVariantSelect && len(m.variants) > 1 {
			m.State = stateVariantSelect
			m.selectedVariant = nil
			m.PullStatus = ""
		} else {
			m.State = StateList
			m.selectedItem = nil
			m.selectedVariant = nil
			m.renderedDesc = ""
			m.PullStatus = ""
		}

	default:
		// Scroll navigation (up/down/pgup/pgdn/home/end)
		if shared.DetailScroll(&m.detailScroll, shared.VisibleRows(m.TermHeight, shared.StdListFooter(m.styles)),
			msg, tui.SearchSubKeys.Up, tui.SearchSubKeys.Down, tui.SearchSubKeys.PageUp, tui.SearchSubKeys.PageDown, tui.SearchSubKeys.Home, tui.SearchSubKeys.End) {
			return m, nil
		}

		ch := msg.String()
		switch ch {
		case "p", "P":
			if m.selectedItem == nil {
				break
			}
			m.PullStatus = "Validating model..."
			return m, m.validateModelForDeploy(m.selectedItem.Provider, m.selectedItem.APIName())
		case "m", "M":
			if m.embedded {
				return m, func() tea.Msg { return tui.SwitchToModelsMsg{} }
			}
		}
	}

	return m, nil
}

// updateVariantSelect handles variant selection view
func (m SearchTUIModel) updateVariantSelect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = StateList
		m.variantCursor = 0
		m.variantOffset = 0
		m.selectedVariant = nil

	default:
		nav := shared.ListNav{Cursor: &m.variantCursor, Offset: &m.variantOffset, Total: len(m.variants), Vis: m.variantPageSize()}
		if nav.Navigate(msg, tui.SearchSubKeys.Up, tui.SearchSubKeys.Down, tui.SearchSubKeys.PageUp, tui.SearchSubKeys.PageDown, tui.SearchSubKeys.Home, tui.SearchSubKeys.End) {
			return m, nil
		}

		switch {
		case key.Matches(msg, tui.SearchSubKeys.Enter):
			if m.variantCursor < len(m.variants) {
				m.selectedVariant = &m.variants[m.variantCursor]
				m.prevState = stateVariantSelect
				m.State = StateDetail
				return m, m.fetchModelDetails()
			}

		case key.Matches(msg, tui.SearchKeys.Sort):
			sortOptions := []string{"recommended", "name", "size"}
			m.pickerTitle = "Sort variants"
			m.pickerItems = sortOptions
			m.pickerKeys = sortOptions
			m.pickerCursor = 0
			current := m.variantSort
			if current == "" {
				current = "recommended"
			}
			for i, s := range sortOptions {
				if s == current {
					m.pickerCursor = i
					break
				}
			}
			m.pickerAction = "variant-sort"
			m.prevState = m.State
			m.State = statePickerOverlay
		}
	}

	return m, nil
}

// updateProviderSelect handles provider selection view
func (m SearchTUIModel) updateProviderSelect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = StateDetail
		m.providerCursor = 0

	case key.Matches(msg, tui.SearchSubKeys.Up):
		if m.providerCursor > 0 {
			m.providerCursor--
		}

	case key.Matches(msg, tui.SearchSubKeys.Down):
		if m.providerCursor < len(m.providerOptions)-1 {
			m.providerCursor++
		}

	case key.Matches(msg, tui.SearchSubKeys.Enter):
		if m.selectedItem == nil {
			m.State = StateDetail
			m.PullStatus = formatErrorMessage("No model selected")
			return m, nil
		}
		if m.providerCursor < len(m.providerOptions) {
			m.selectedProvider = m.providerOptions[m.providerCursor].Provider
			m.PullStatus = "Checking available nodes..."
			return m, m.loadAvailableNodes()
		}
	}

	return m, nil
}

// updateDeploying handles pulling state
func (m SearchTUIModel) updateDeploying(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, tui.SearchSubKeys.Quit) {
		return m, m.quitOrBack()
	}
	return m, nil
}

// updateFilterEdit handles filter edit state
func (m SearchTUIModel) updateFilterEdit(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = StateList
		return m, nil

	case key.Matches(msg, tui.SearchSubKeys.Enter):
		m.query = m.filterInput
		m.State = StateLoading
		m.cursor = 0
		m.listOffset = 0
		return m, m.fetchResults()

	case key.Matches(msg, tui.SearchKeys.Backspace):
		if len(m.filterInput) > 0 {
			m.filterInput = m.filterInput[:len(m.filterInput)-1]
		}

	default:
		ch := msg.String()
		if len(ch) == 1 && ch[0] >= 32 && ch[0] < 127 {
			m.filterInput += ch
		}
	}

	return m, nil
}

// updateNodeSelect handles host selection state
func (m SearchTUIModel) updateNodeSelect(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if len(m.availableNodes) == 0 {
		if key.Matches(msg, tui.SearchSubKeys.Quit) {
			return m, m.quitOrBack()
		}
		if key.Matches(msg, tui.SearchSubKeys.Back) {
			m.State = StateDetail
			return m, nil
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = StateDetail
		m.PullStatus = ""
		return m, nil

	case key.Matches(msg, tui.SearchSubKeys.Up):
		if m.hostCursor > 0 {
			m.hostCursor--
		}

	case key.Matches(msg, tui.SearchSubKeys.Down):
		if m.hostCursor < len(m.availableNodes)-1 {
			m.hostCursor++
		}

	case key.Matches(msg, tui.SearchSubKeys.Space):
		if m.hostCursor >= 0 && m.hostCursor < len(m.availableNodes) {
			name := m.availableNodes[m.hostCursor].name
			m.selectedNodes[name] = !m.selectedNodes[name]
		}

	case key.Matches(msg, tui.SearchSubKeys.Enter):
		if m.selectedItem == nil {
			m.State = StateDetail
			m.PullStatus = formatErrorMessage("No model selected")
			return m, nil
		}
		var nodes []string
		for _, h := range m.availableNodes {
			if m.selectedNodes[h.name] {
				nodes = append(nodes, h.name)
			}
		}
		if len(nodes) == 0 {
			m.PullStatus = formatErrorMessage("No nodes selected")
			return m, nil
		}
		m.State = statePulling
		return m, m.deployModelToNodes(nodes)
	}

	return m, nil
}

// updatePickerOverlay handles input in the picker popup.
func (m SearchTUIModel) updatePickerOverlay(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = m.prevState

	case key.Matches(msg, tui.SearchSubKeys.Up):
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}

	case key.Matches(msg, tui.SearchSubKeys.Down):
		if m.pickerCursor < len(m.pickerItems)-1 {
			m.pickerCursor++
		}

	case key.Matches(msg, tui.SearchSubKeys.Enter):
		if m.pickerCursor >= len(m.pickerKeys) {
			break
		}
		switch m.pickerAction {
		case "sort":
			m.sortBy = m.pickerKeys[m.pickerCursor]
			m.cursor = 0
			m.listOffset = 0
			m.State = m.prevState
			cfg := m.getProviderConfig(m.Provider)
			if cfg.ClientSorts[m.sortBy] {
				m.results = applyClientSort(m.results, m.sortBy)
				m.allResults = applyClientSort(m.allResults, m.sortBy)
			} else {
				m.State = StateLoading
				return m, m.fetchResults()
			}
			go saveSearchPrefs(m.Provider, m.sortBy, m.tagFilter)

		case "provider":
			m.Provider = m.pickerKeys[m.pickerCursor]
			m.allResults = nil // clear cache for new provider
			m.State = StateLoading
			m.cursor = 0
			m.listOffset = 0
			// Restore per-provider sort and tags from saved prefs
			m.sortBy = ""
			m.tagFilter = nil
			m.tagPresetIndex = -1
			m.tagLogic = m.tagDefaultLogic
			if prefs := loadRegistryPrefs(m.Provider); prefs != nil {
				m.sortBy = prefs.Sort
				m.tagFilter = prefs.Tags
			}
			go saveSearchPrefs(m.Provider, m.sortBy, m.tagFilter)
			return m, m.fetchResults()

		case "tags":
			selected := m.pickerKeys[m.pickerCursor]
			if selected == pickerKeyNone {
				m.tagPresetIndex = -1
				m.tagFilter = nil
				m.tagLogic = m.tagDefaultLogic
			} else if selected == "local" || selected == "cloud" {
				// Ollama built-in cloud/local filter
				m.tagPresetIndex = m.pickerCursor - 1
				m.tagFilter = []string{selected}
				m.tagLogic = "AND"
			} else {
				// User-defined tag presets from config
				// Offset: skip "All" + any built-in tags (Ollama has 2)
				builtinCount := 0
				if m.Provider == constants.RepoOllama {
					builtinCount = 2
				}
				presetIdx := m.pickerCursor - 1 - builtinCount
				m.tagPresetIndex = m.pickerCursor - 1
				if presetIdx >= 0 && presetIdx < len(m.tagPresets) {
					preset := m.tagPresets[presetIdx]
					m.tagFilter = append([]string(nil), preset.Tags...)
					m.tagLogic = preset.Logic
				}
			}
			m.State = StateLoading
			m.cursor = 0
			m.listOffset = 0
			go saveSearchPrefs(m.Provider, m.sortBy, m.tagFilter)
			return m, m.fetchResults()

		case "variant-sort":
			m.variantSort = m.pickerKeys[m.pickerCursor]
			if m.variantSort != "recommended" {
				sortVariantsByField(m.variants, m.variantSort)
			} else {
				sortVariants(m.variants)
			}
			m.variantCursor = 0
			m.variantOffset = 0
			m.State = m.prevState
		}

	default:
		// Tag preset management: 'a' to add, 'd' to delete
		if m.pickerAction == "tags" {
			ch := msg.String()
			switch ch {
			case "a", "A":
				m.tagAddInput = ""
				m.State = stateTagAdd
				return m, nil
			case "d", "D", "x", "X":
				// Can't delete "All (no filter)" or Ollama built-ins
				if m.pickerCursor == 0 {
					break
				}
				builtinCount := 0
				if m.Provider == constants.RepoOllama {
					builtinCount = 2
					if m.pickerCursor <= builtinCount {
						break
					}
				}
				presetIdx := m.pickerCursor - 1 - builtinCount
				if presetIdx >= 0 && presetIdx < len(m.tagPresets) {
					m.tagPresets = append(m.tagPresets[:presetIdx], m.tagPresets[presetIdx+1:]...)
					go saveTagPresets(m.tagPresets)
					m.rebuildTagPickerItems()
					if m.pickerCursor >= len(m.pickerItems) {
						m.pickerCursor = len(m.pickerItems) - 1
					}
					// Reset active filter if we deleted it
					if m.tagPresetIndex == presetIdx {
						m.tagPresetIndex = -1
						m.tagFilter = nil
						m.tagLogic = m.tagDefaultLogic
					} else if m.tagPresetIndex > presetIdx {
						m.tagPresetIndex--
					}
				}
				return m, nil
			}
		}
	}

	return m, nil
}

// updateDirectDeploy handles the direct deploy overlay (Ctrl+D).
// User types a full model name and presses Enter to validate, then deploy.
func (m SearchTUIModel) updateDirectDeploy(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.directPullLoading {
		return m, nil // Ignore input while validating
	}

	switch {
	case key.Matches(msg, tui.SearchSubKeys.Quit):
		return m, m.quitOrBack()

	case key.Matches(msg, tui.SearchSubKeys.Back):
		m.State = m.prevState
		m.directPullInput = ""
		m.directPullError = ""
		return m, nil

	default:
		ch := msg.String()
		switch ch {
		case "enter":
			modelName := strings.TrimSpace(m.directPullInput)
			if modelName == "" {
				break
			}
			m.directPullError = ""
			m.directPullLoading = true
			return m, m.validateModelForDeploy(m.Provider, modelName)

		case "backspace":
			if len(m.directPullInput) > 0 {
				m.directPullInput = m.directPullInput[:len(m.directPullInput)-1]
				m.directPullError = "" // Clear error on edit
			}
		default:
			if len(ch) == 1 && ch[0] >= 32 {
				m.directPullInput += ch
				m.directPullError = "" // Clear error on edit
			}
		}
	}
	return m, nil
}

// updateTagAdd handles text input for adding a new tag preset.
func (m SearchTUIModel) updateTagAdd(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.SearchSubKeys.Back):
		// Cancel — return to picker
		m.State = statePickerOverlay
		m.tagAddInput = ""

	case key.Matches(msg, tui.SearchSubKeys.Enter):
		tag := strings.TrimSpace(m.tagAddInput)
		if tag != "" {
			m.tagPresets = append(m.tagPresets, TagPreset{Tags: []string{tag}, Logic: m.tagDefaultLogic})
			go saveTagPresets(m.tagPresets)
			m.rebuildTagPickerItems()
			m.pickerCursor = len(m.pickerItems) - 1 // select the newly added tag
		}
		m.State = statePickerOverlay
		m.tagAddInput = ""

	default:
		ch := msg.String()
		if ch == "backspace" {
			if len(m.tagAddInput) > 0 {
				m.tagAddInput = m.tagAddInput[:len(m.tagAddInput)-1]
			}
		} else if len(ch) == 1 && ch >= " " {
			m.tagAddInput += ch
		}
	}
	return m, nil
}

// isOllamaCloudTag returns true if the model name contains a cloud tag suffix.
func isOllamaCloudTag(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ":cloud") || strings.Contains(lower, ":cloud-")
}

// buildIncompatPopup constructs a ui.Popup for an "no compatible provider"
// error returned by modelregistry.FormatIncompatibleError. The copy points
// users at the Providers menu in the TUI — the only supported managed path.
func buildIncompatPopup(errMsg string) *ui.Popup {
	return &ui.Popup{
		Title:    "Provider not installed",
		Message:  friendlyIncompatMessage(errMsg),
		Severity: ui.PopupError,
		Hints:    "Esc/Enter dismiss",
	}
}

// friendlyIncompatMessage rewrites FormatIncompatibleError into user-facing
// copy that names the runtime and directs the user to the Providers menu.
// Falls back to a generic message if the detected format isn't recognized.
func friendlyIncompatMessage(errMsg string) string {
	lower := strings.ToLower(errMsg)
	switch {
	case strings.Contains(lower, "detected format: mlx"):
		return "This model needs the MLX runtime, and no node in the cluster has it installed yet. " +
			"Open the Providers menu in the TUI to install MLX on a node."
	case strings.Contains(lower, "detected format: gguf"):
		return "This model needs a GGUF-compatible runtime (llama.cpp or Ollama), " +
			"and no node in the cluster has one installed yet. " +
			"Open the Providers menu in the TUI to install one on a node."
	case strings.Contains(lower, "detected format: safetensors"):
		return "This model needs a safetensors-compatible runtime (vLLM), " +
			"and no node in the cluster has it installed yet. " +
			"Open the Providers menu in the TUI to install vLLM on a node."
	}
	return "No installed provider on any node can serve this model. " +
		"Open the Providers menu in the TUI to install one."
}
