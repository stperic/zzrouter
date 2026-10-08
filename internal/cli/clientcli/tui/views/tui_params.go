package views

import (
	"errors"
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

// ============================================================================
// Parameter Editor View — API Integration Layer
// ============================================================================
//
// Wraps KVEditorModel and connects it to the parameter management API.
//
// Two modes:
//   - App mode (no model): two tabs — "App Defaults" and "Node Overrides"
//   - Model mode (model selected): single merged view (app → node → model)
//     with source labels and resolve hints. Edits save as model overrides only.

// paramsTab tracks which tab is active in app mode.
type paramsTab int

const (
	paramsTabProviderDefaults paramsTab = iota // App-level defaults
	paramsTabNode                              // Tier 2 — node-level parameters
)

// paramsLoadedMsg carries the API response.
type paramsLoadedMsg struct {
	providerParams *pkgClient.ProviderParametersResponse
	nodeParams     *pkgClient.NodeParametersResponse  // nil for model mode
	modelParams    *pkgClient.ModelParametersResponse // nil for provider mode
	nodeName       string                             // coordinator node name (for node tab)
	err            error
}

// paramsSavedMsg is the result of a save operation.
type paramsSavedMsg struct {
	resp *pkgClient.UpdateParametersResponse
	err  error
}

// paramsValidatedMsg is the result of a dry_run validation.
type paramsValidatedMsg struct {
	resp *pkgClient.UpdateParametersResponse
	err  error
}

// paramsDeletedMsg is the result of a single-param delete.
type paramsDeletedMsg struct {
	key string
	err error
}

// paramsIgnoredMsg reports the outcome of an ignore keypress. It only
// ever carries an error now: there is no ignored state left to report.
type paramsIgnoredMsg struct {
	key string
	err error
}

type ParamsViewModel struct {
	client   *pkgClient.Client
	styles   ui.Styles
	Provider string
	Model    string // empty for app mode

	// Tabs (app mode only — model mode has no tabs)
	activeTab paramsTab

	// Editors — one per tab context
	providerEditor KVEditorModel // app-level defaults (app mode) or unused (model mode)
	nodeEditor     KVEditorModel // node-level overrides (app mode only)
	modelEditor    KVEditorModel // merged model view (model mode only)

	// Coordinator node name (for node overrides tab)
	nodeName string

	// App defaults cache (for building model-mode DefaultValue)
	providerDefaults map[string]string

	// Cached tab styles (avoid allocating every render)
	tabActiveStyle   lipgloss.Style
	tabInactiveStyle lipgloss.Style

	loading    bool
	err        error
	termWidth  int
	termHeight int
}

func NewParamsViewModel(client *pkgClient.Client, styles ui.Styles, provider, model string) *ParamsViewModel {
	return &ParamsViewModel{
		client:   client,
		styles:   styles,
		Provider: provider,
		Model:    model,
		loading:  true,
		tabActiveStyle: lipgloss.NewStyle().
			Bold(true).
			Foreground(styles.Theme.Primary).
			Underline(true),
		tabInactiveStyle: lipgloss.NewStyle().
			Foreground(styles.Theme.Subtext),
	}
}

func (m *ParamsViewModel) IsModelMode() bool {
	return m.Model != ""
}

// Init implements tea.Model.
func (m *ParamsViewModel) Init() tea.Cmd { return m.Refresh() }

// Refresh implements tui.Refresher: re-resolve the parameter tiers, which a
// child view may have written to.
func (m *ParamsViewModel) Refresh() tea.Cmd {
	client := m.client
	provider := m.Provider
	model := m.Model

	return func() tea.Msg {
		if model != "" {
			// Model mode: single merged view with resolve hints
			modelResp, err := client.GetModelParameters(provider, model, true)
			if err != nil {
				return paramsLoadedMsg{err: err}
			}
			return paramsLoadedMsg{modelParams: modelResp}
		}

		// Provider mode: fetch provider params, node name, then node params
		provResp, err := client.GetProviderParameters(provider, true)
		if err != nil {
			return paramsLoadedMsg{err: err}
		}

		nodeName, err := client.GetCoordinatorNodeName()
		if err != nil {
			// Non-fatal: node tab will be unavailable
			return paramsLoadedMsg{
				providerParams: provResp,
				nodeName:       "",
			}
		}

		nodeResp, err := client.GetNodeParameters(provider, nodeName, true)
		if err != nil {
			// Non-fatal: node tab will be unavailable
			return paramsLoadedMsg{
				providerParams: provResp,
				nodeName:       nodeName,
			}
		}

		return paramsLoadedMsg{
			providerParams: provResp,
			nodeParams:     nodeResp,
			nodeName:       nodeName,
		}
	}
}

// Update implements tea.Model.
func (m *ParamsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		editorMsg := tea.WindowSizeMsg{Width: msg.Width, Height: m.editorHeight()}
		m.providerEditor, _ = m.providerEditor.update(editorMsg)
		m.nodeEditor, _ = m.nodeEditor.update(editorMsg)
		m.modelEditor, _ = m.modelEditor.update(editorMsg)
		return m, nil

	case paramsLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.initEditors(msg)
		return m, nil

	case kvSaveMsg:
		return m, m.handleSave(msg)

	case kvValidateMsg:
		return m, m.handleValidate(msg)

	case kvDeleteMsg:
		return m, m.handleDelete(msg)

	case kvIgnoreMsg:
		return m, m.handleIgnore(msg)

	case kvCancelMsg:
		return m, tui.NavBack()

	case paramsSavedMsg:
		m.handleSaveResult(msg)
		return m, nil

	case paramsValidatedMsg:
		m.handleValidateResult(msg)
		return m, nil

	case paramsDeletedMsg:
		if msg.err != nil {
			m.activeEditor().statusMsg = fmt.Sprintf("Delete failed: %v", msg.err)
			m.activeEditor().statusError = true
			return m, nil
		}
		return m, m.Init()

	case paramsIgnoredMsg:
		if msg.err != nil {
			m.activeEditor().statusMsg = fmt.Sprintf("Ignore toggle failed: %v", msg.err)
			m.activeEditor().statusError = true
			return m, nil
		}
		// Refresh from API to get updated state
		return m, m.Init()

	case tea.KeyPressMsg:
		// Tab switching in app mode (not model mode)
		if !m.IsModelMode() && key.Matches(msg, tui.KvOverlayKeys.Tab) {
			editor := m.activeEditor()
			if editor.mode == kvModeList {
				if m.activeTab == paramsTabProviderDefaults {
					m.activeTab = paramsTabNode
				} else {
					m.activeTab = paramsTabProviderDefaults
				}
				return m, nil
			}
		}
	}

	// Forward all unhandled messages to active editor
	return m, m.forwardToEditor(msg)
}

func (m *ParamsViewModel) forwardToEditor(msg tea.Msg) tea.Cmd {
	// Pre-capture the full view as overlay background before the editor processes
	// the message (which might open an overlay). This includes tabs + editor list
	// so the dimmed background shows the complete params screen.
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		if key.Matches(keyMsg, tui.KvEditorKeys.Enter, tui.KvEditorKeys.Add) {
			editor := m.activeEditor()
			editor.setFullBackground(m.viewContent())
		}
	}

	var cmd tea.Cmd
	if m.IsModelMode() {
		m.modelEditor, cmd = m.modelEditor.update(msg)
	} else if m.activeTab == paramsTabNode {
		m.nodeEditor, cmd = m.nodeEditor.update(msg)
	} else {
		m.providerEditor, cmd = m.providerEditor.update(msg)
	}
	return cmd
}

func (m *ParamsViewModel) activeEditor() *KVEditorModel {
	if m.IsModelMode() {
		return &m.modelEditor
	}
	if m.activeTab == paramsTabNode {
		return &m.nodeEditor
	}
	return &m.providerEditor
}

func (m *ParamsViewModel) initEditors(msg paramsLoadedMsg) {
	m.providerDefaults = make(map[string]string)

	if m.IsModelMode() {
		// Model mode: single merged view
		if msg.modelParams != nil {
			// Build app defaults map from entries with source="provider"
			for _, p := range msg.modelParams.Parameters {
				if p.Source == "provider" || p.Source == "node" {
					m.providerDefaults[p.Key] = p.Value
				}
			}

			modelEntries := paramEntriesToKVEntries(msg.modelParams.Parameters, true, m.providerDefaults, m.Provider)
			envEntries := envEntriesToKVEntries(msg.modelParams.Environment, true)
			allEntries := append(modelEntries, envEntries...)
			m.modelEditor = newKVEditor(allEntries, m.Model+" Parameters", true, m.styles)
			m.modelEditor.width = m.termWidth
			m.modelEditor.height = m.editorHeight()
		}
		return
	}

	// App mode: app defaults + node overrides
	if msg.providerParams != nil {
		for _, p := range msg.providerParams.Parameters {
			m.providerDefaults[p.Key] = p.Value
		}
	}

	providerEntries := paramEntriesToKVEntries(msg.providerParams.Parameters, false, nil, "")
	providerEnvEntries := envEntriesToKVEntries(msg.providerParams.Environment, false)
	allProviderEntries := append(providerEntries, providerEnvEntries...)
	m.providerEditor = newKVEditor(allProviderEntries, m.Provider+" Provider Defaults", false, m.styles)
	m.providerEditor.width = m.termWidth
	m.providerEditor.height = m.editorHeight()

	m.nodeName = msg.nodeName
	if msg.nodeParams != nil {
		nodeEntries := paramEntriesToKVEntries(msg.nodeParams.Parameters, true, m.providerDefaults, m.Provider)
		nodeEnvEntries := envEntriesToKVEntries(msg.nodeParams.Environment, true)
		allNodeEntries := append(nodeEntries, nodeEnvEntries...)
		nodeTitle := m.Provider + " Node Overrides"
		if m.nodeName != "" {
			nodeTitle += " (" + m.nodeName + ")"
		}
		m.nodeEditor = newKVEditor(allNodeEntries, nodeTitle, true, m.styles)
		m.nodeEditor.width = m.termWidth
		m.nodeEditor.height = m.editorHeight()
	}
}

// paramsTabBarLines is the overhead for tab bar + note + separator in app mode.
const paramsTabBarLines = 3

func (m *ParamsViewModel) editorHeight() int {
	h := m.termHeight - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
	if !m.IsModelMode() {
		h -= paramsTabBarLines
	}
	return h
}

// ============================================================================
// API Handlers
// ============================================================================

// buildUpdateRequest converts editor entries to an API request,
// splitting entries by section into Parameters and Environment maps.
func buildUpdateRequest(entries []KVEntry) *pkgClient.UpdateParametersRequest {
	req := &pkgClient.UpdateParametersRequest{
		Parameters:  make(map[string]string),
		Environment: make(map[string]string),
	}
	for _, e := range entries {
		if e.Section == kvSectionEnvironment {
			req.Environment[e.Key] = e.Value
		} else {
			req.Parameters[e.Key] = e.Value
		}
	}
	return req
}

// applyWarnings maps validation warnings onto editor entries.
func applyWarnings(entries []KVEntry, warnings []pkgClient.ValidationWarning) int {
	// Clear existing warnings
	for i := range entries {
		entries[i].Warnings = nil
	}
	if len(warnings) == 0 {
		return 0
	}
	warnMap := make(map[string][]string, len(warnings))
	for _, w := range warnings {
		warnMap[w.Key] = append(warnMap[w.Key], w.Message)
	}
	for i := range entries {
		if w, ok := warnMap[entries[i].Key]; ok {
			entries[i].Warnings = w
		}
	}
	return len(warnings)
}

func (m *ParamsViewModel) handleSave(msg kvSaveMsg) tea.Cmd {
	client := m.client
	provider := m.Provider
	model := m.Model
	tab := m.activeTab
	nodeName := m.nodeName
	isModel := m.IsModelMode()
	req := buildUpdateRequest(msg.Entries)

	return func() tea.Msg {
		var resp *pkgClient.UpdateParametersResponse
		var err error

		if isModel {
			resp, err = client.UpdateModelParameters(provider, model, req, false)
		} else if tab == paramsTabNode && nodeName != "" {
			resp, err = client.UpdateNodeParameters(provider, nodeName, req, false)
		} else {
			resp, err = client.UpdateProviderParameters(provider, req, false)
		}
		return paramsSavedMsg{resp: resp, err: err}
	}
}

func (m *ParamsViewModel) handleValidate(msg kvValidateMsg) tea.Cmd {
	client := m.client
	provider := m.Provider
	model := m.Model
	tab := m.activeTab
	nodeName := m.nodeName
	isModel := m.IsModelMode()
	req := buildUpdateRequest(msg.Entries)

	return func() tea.Msg {
		var resp *pkgClient.UpdateParametersResponse
		var err error

		if isModel {
			resp, err = client.UpdateModelParameters(provider, model, req, true)
		} else if tab == paramsTabNode && nodeName != "" {
			resp, err = client.UpdateNodeParameters(provider, nodeName, req, true)
		} else {
			resp, err = client.UpdateProviderParameters(provider, req, true)
		}
		return paramsValidatedMsg{resp: resp, err: err}
	}
}

func (m *ParamsViewModel) handleDelete(msg kvDeleteMsg) tea.Cmd {
	client := m.client
	provider := m.Provider
	model := m.Model
	tab := m.activeTab
	nodeName := m.nodeName
	isModel := m.IsModelMode()
	deletedKey := msg.Key
	isEnv := msg.Section == kvSectionEnvironment

	return func() tea.Msg {
		var err error
		if isModel {
			if isEnv {
				err = client.DeleteModelEnvironment(provider, model, deletedKey)
			} else {
				err = client.DeleteModelParameter(provider, model, deletedKey)
			}
		} else if tab == paramsTabNode && nodeName != "" {
			if isEnv {
				err = client.DeleteNodeEnvironment(provider, nodeName, deletedKey)
			} else {
				err = client.DeleteNodeParameter(provider, nodeName, deletedKey)
			}
		} else {
			if isEnv {
				err = client.DeleteProviderEnvironment(provider, deletedKey)
			} else {
				err = client.DeleteProviderParameter(provider, deletedKey)
			}
		}
		return paramsDeletedMsg{key: deletedKey, err: err}
	}
}

// handleIgnore reports that per-key ignore is gone.
//
// The server retired the PATCH .../ignore routes when Merge-Patch became
// the single mutator, and the "ignored" flag no longer exists in provider
// config at all, so there is nothing left to toggle. The key binding
// still lives in the shared kv editor; removing it is a TUI change that
// wants its own review, and until then saying so beats the silent 410
// this replaced.
func (m *ParamsViewModel) handleIgnore(msg kvIgnoreMsg) tea.Cmd {
	key := msg.Key
	return func() tea.Msg {
		return paramsIgnoredMsg{
			key: key,
			err: errors.New("ignoring a single parameter is no longer supported; clear its value instead"),
		}
	}
}

func (m *ParamsViewModel) handleSaveResult(msg paramsSavedMsg) {
	editor := m.activeEditor()
	if msg.err != nil {
		editor.statusMsg = fmt.Sprintf("Save failed: %v", msg.err)
		editor.statusError = true
		return
	}

	if msg.resp != nil {
		var entries []KVEntry
		if m.IsModelMode() {
			entries = paramEntriesToKVEntries(msg.resp.Parameters, true, m.providerDefaults, m.Provider)
			entries = append(entries, envEntriesToKVEntries(msg.resp.Environment, true)...)
		} else if m.activeTab == paramsTabNode {
			entries = paramEntriesToKVEntries(msg.resp.Parameters, true, m.providerDefaults, m.Provider)
			entries = append(entries, envEntriesToKVEntries(msg.resp.Environment, true)...)
		} else {
			entries = paramEntriesToKVEntries(msg.resp.Parameters, false, nil, "")
			entries = append(entries, envEntriesToKVEntries(msg.resp.Environment, false)...)
			for _, p := range msg.resp.Parameters {
				m.providerDefaults[p.Key] = p.Value
			}
		}
		editor.entries = entries
		editor.dirty = false

		n := applyWarnings(editor.entries, msg.resp.Warnings)
		if n > 0 {
			editor.statusMsg = fmt.Sprintf("Saved with %d warning(s)", n)
		} else {
			editor.statusMsg = "Saved"
		}
		editor.statusError = false
	}
}

func (m *ParamsViewModel) handleValidateResult(msg paramsValidatedMsg) {
	editor := m.activeEditor()
	if msg.err != nil {
		editor.statusMsg = fmt.Sprintf("Validation error: %v", msg.err)
		editor.statusError = true
		return
	}

	var warnings []pkgClient.ValidationWarning
	if msg.resp != nil {
		warnings = msg.resp.Warnings
	}
	n := applyWarnings(editor.entries, warnings)
	if n > 0 {
		editor.statusMsg = fmt.Sprintf("%d warning(s)", n)
	} else {
		editor.statusMsg = "Validation passed"
	}
	editor.statusError = false
}

// ============================================================================
// View
// ============================================================================

// Breadcrumb implements tui.Breadcrumber.
func (m *ParamsViewModel) Breadcrumb() []string {
	if m.IsModelMode() {
		return []string{"Parameters", m.Model}
	}
	return []string{"Parameters", m.Provider}
}

// View implements tea.Model.
func (m *ParamsViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *ParamsViewModel) viewContent() string {
	var b strings.Builder
	width := m.termWidth

	if m.loading {
		b.WriteString(" Loading...\n")
		return b.String()
	}

	if m.err != nil {
		b.WriteString(shared.WrapIndent(m.styles.Error.Render(fmt.Sprintf("Error: %v", m.err)), " ", width))
		b.WriteString("\n")
		return b.String()
	}

	if m.IsModelMode() {
		// Model mode: single merged view, no tabs
		b.WriteString(m.modelEditor.View(width, m.editorHeight()))
		return b.String()
	}

	// App mode: show tabs only when viewing node overrides
	if m.activeTab == paramsTabNode {
		appTab := " Default Parameters "
		nodeTab := " Node Overrides "
		b.WriteString(" " + m.tabInactiveStyle.Render(appTab) + "  " + m.tabActiveStyle.Render(nodeTab))
		b.WriteString("\n")
		if m.nodeName != "" {
			b.WriteString(" " + m.styles.Help.Render(fmt.Sprintf("Overrides for node %s only", m.nodeName)))
			b.WriteString("\n")
		}
	}

	editorHeight := m.editorHeight()
	if m.activeTab == paramsTabNode {
		b.WriteString(m.nodeEditor.View(width, editorHeight))
	} else {
		b.WriteString(m.providerEditor.View(width, editorHeight))
	}

	return b.String()
}

// ============================================================================
// Translation: ParameterEntry → KVEntry
// ============================================================================

func paramEntriesToKVEntries(params []pkgClient.ParameterEntry, overrideMode bool, providerDefaults map[string]string, provider string) []KVEntry {
	entries := make([]KVEntry, 0, len(params))
	for _, p := range params {
		e := KVEntry{
			Key:              p.Key,
			Value:            p.Value,
			Section:          kvSectionParameter,
			Source:           p.Source,
			Label:            p.Label,
			Description:      p.Description,
			InputType:        p.InputType,
			Options:          p.Options,
			SupportsAuto:     p.SupportsAuto,
			IsOverride:       p.IsOverride,
			AutoResolvedHint: p.AutoResolvedHint,
			Warnings:         p.Warnings,
			Ignored:          p.Ignored,
		}
		if overrideMode && providerDefaults != nil {
			if def, ok := providerDefaults[p.Key]; ok {
				e.DefaultValue = def
				e.DefaultSource = provider
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// envEntriesToKVEntries converts parameter entries to KV editor rows.
// overrideMode was used for deprecated row styling and is kept for call-site
// symmetry with the three distinct ParameterEntry slice sources; not read.
func envEntriesToKVEntries(env []pkgClient.ParameterEntry, _ bool) []KVEntry {
	entries := make([]KVEntry, 0, len(env))
	for _, p := range env {
		entries = append(entries, KVEntry{
			Key:              p.Key,
			Value:            p.Value,
			Section:          kvSectionEnvironment,
			Source:           p.Source,
			Label:            p.Label,
			Description:      p.Description,
			InputType:        p.InputType,
			Options:          p.Options,
			SupportsAuto:     p.SupportsAuto,
			IsOverride:       p.IsOverride,
			AutoResolvedHint: p.AutoResolvedHint,
			Warnings:         p.Warnings,
			Ignored:          p.Ignored,
		})
	}
	return entries
}
