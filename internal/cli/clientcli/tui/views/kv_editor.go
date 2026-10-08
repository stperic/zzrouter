package views

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ============================================================================
// Key-Value Editor — Reusable Component
// ============================================================================
//
// A generic, API-agnostic key-value list editor with overlay value editing.
// Used by ParamsViewModel to edit provider and model parameters.
//
// State machine:
//   kvModeList → Enter → kvModeEditValue (overlay)
//   kvModeList → A     → kvModeAddNew (overlay)
//   kvModeList → D     → kvModeConfirm (inline)
//   kvModeList → Esc   → kvModeConfirm (if dirty) or kvCancelMsg
//
// The editor emits messages (kvSaveMsg, kvDeleteMsg, etc.) for the parent
// to handle via API calls. The parent mutates editor state directly.

// KVEntry section constants.
const (
	kvSectionParameter   = "parameter"
	kvSectionEnvironment = "environment"
)

// KVEntry is a single key-value pair for the editor.
type KVEntry struct {
	Key              string
	Value            string
	Section          string   // "parameter" or "environment"
	Source           string   // "provider", "model", "custom"
	Label            string   // human-readable name (from schema)
	Description      string   // longer description for overlay
	InputType        string   // "text", "number", "boolean", "select"
	Options          []string // valid options for select-style fields
	SupportsAuto     bool
	IsOverride       bool   // true if this is a model-level override
	AutoResolvedHint string // resolved value for "auto" or "${VAR}" expressions
	DefaultValue     string // provider default value (for model mode display)
	DefaultSource    string // e.g. "vllm" (for "default (X — from vllm)" text)
	Warnings         []string
	ReadOnly         bool
	Ignored          bool
}

// kvEditorMode tracks the current interaction state.
type kvEditorMode int

const (
	kvModeList      kvEditorMode = iota // browsing the list
	kvModeEditValue                     // overlay open, editing a value
	kvModeAddNew                        // overlay open, adding new key+value
	kvModeConfirm                       // delete or discard confirmation
)

// kvConfirmAction identifies what a confirmation dialog is confirming.
const (
	kvConfirmDelete  = "delete"
	kvConfirmDiscard = "discard"
)

// kvRadioChoice identifies the type of radio item in the edit overlay.
type kvRadioChoice int

const (
	kvChoiceDefault kvRadioChoice = iota // "default (X — from provider)"
	kvChoiceAuto                         // "auto"
	kvChoiceOption                       // one of the predefined Options
	kvChoiceCustom                       // free-text via textinput
	kvChoiceIgnore                       // "ignore — won't be passed at launch"
)

// kvRadioItem is one choice in the overlay radio list.
type kvRadioItem struct {
	label  string        // display text
	choice kvRadioChoice // which type
	value  string        // the actual value this represents
}

// KVEditorModel is the reusable key-value editor component.
type KVEditorModel struct {
	entries   []KVEntry
	title     string
	cursor    int
	offset    int
	mode      kvEditorMode
	dirty     bool
	modelMode bool // true = show default/override semantics

	// Overlay state (edit value)
	overlayEntry   *KVEntry      // the entry being edited (nil for add)
	overlayChoices []kvRadioItem // computed radio items for current entry
	overlayCursor  int           // which radio item is selected
	overlayInput   textinput.Model

	// Overlay state (add new)
	addKeyInput   textinput.Model
	addValueInput textinput.Model
	addFocusKey   bool   // true = key field focused, false = value field
	addSection    string // "parameter" or "environment"

	// Confirm state
	confirmMessage string
	confirmAction  string // "delete", "discard"
	confirmIndex   int

	// Status
	statusMsg   string
	statusError bool

	// Display
	styles       ui.Styles
	warningStyle lipgloss.Style
	hintStyle    lipgloss.Style
	width        int
	height       int

	// Cached list render for overlay background
	cachedListBg string
}

// Messages emitted to parent
type kvSaveMsg struct{ Entries []KVEntry }
type kvValidateMsg struct{ Entries []KVEntry }
type kvDeleteMsg struct {
	Key     string
	Section string // "parameter" or "environment"
}
type kvIgnoreMsg struct {
	Key     string
	Section string // "parameter" or "environment"
}
type kvCancelMsg struct{}

func newKVEditor(entries []KVEntry, title string, modelMode bool, styles ui.Styles) KVEditorModel {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 256
	ti.Validate = validateValue

	addKey := textinput.New()
	addKey.Prompt = ""
	addKey.Placeholder = "parameter-name"
	addKey.CharLimit = 128
	addKey.Validate = validateParamKey

	addVal := textinput.New()
	addVal.Prompt = ""
	addVal.Placeholder = "value"
	addVal.CharLimit = 256
	addVal.Validate = validateValue

	return KVEditorModel{
		entries:       entries,
		title:         title,
		modelMode:     modelMode,
		styles:        styles,
		warningStyle:  lipgloss.NewStyle().Foreground(styles.Theme.Warning),
		hintStyle:     lipgloss.NewStyle().Foreground(styles.Theme.Subtext),
		overlayInput:  ti,
		addKeyInput:   addKey,
		addValueInput: addVal,
	}
}

// ============================================================================
// Update
// ============================================================================

func (m KVEditorModel) update(msg tea.Msg) (KVEditorModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.MouseClickMsg:
		if m.mode == kvModeList {
			return m.handleMouseClick(msg)
		}
		return m, nil

	case tea.MouseWheelMsg:
		if m.mode == kvModeList {
			return m.handleMouseWheel(msg)
		}
		return m, nil

	case tea.KeyPressMsg:
		switch m.mode {
		case kvModeList:
			return m.updateList(msg)
		case kvModeEditValue:
			return m.updateEditOverlay(msg)
		case kvModeAddNew:
			return m.updateAddOverlay(msg)
		case kvModeConfirm:
			return m.updateConfirm(msg)
		}
	}

	return m, nil
}

func (m KVEditorModel) updateList(msg tea.KeyPressMsg) (KVEditorModel, tea.Cmd) {
	nav := shared.ListNav{Cursor: &m.cursor, Offset: &m.offset, Total: len(m.entries), Vis: m.VisibleRows()}
	if nav.Navigate(msg, tui.KvEditorKeys.Up, tui.KvEditorKeys.Down, tui.KvEditorKeys.PageUp, tui.KvEditorKeys.PageDown, tui.KvEditorKeys.Home, tui.KvEditorKeys.End) {
		m.statusMsg = ""
		return m, nil
	}

	switch {
	case key.Matches(msg, tui.KvEditorKeys.Enter):
		if m.cursor >= 0 && m.cursor < len(m.entries) {
			entry := &m.entries[m.cursor]
			if entry.ReadOnly {
				return m, nil
			}
			m.openEditOverlay(entry)
		}
		return m, nil

	case key.Matches(msg, tui.KvEditorKeys.Add):
		m.openAddOverlay()
		return m, nil

	case key.Matches(msg, tui.KvEditorKeys.Delete):
		if m.cursor >= 0 && m.cursor < len(m.entries) {
			entry := &m.entries[m.cursor]
			m.mode = kvModeConfirm
			m.confirmAction = kvConfirmDelete
			m.confirmIndex = m.cursor
			if m.modelMode && entry.IsOverride {
				m.confirmMessage = fmt.Sprintf("Remove override for %s? Will revert to default. (y/n)", entry.Key)
			} else {
				m.confirmMessage = fmt.Sprintf("Delete %s? (y/n)", entry.Key)
			}
		}
		return m, nil

	case key.Matches(msg, tui.KvEditorKeys.Validate):
		return m, func() tea.Msg {
			return kvValidateMsg{Entries: m.entries}
		}

	case key.Matches(msg, tui.KvEditorKeys.Save):
		if m.dirty {
			return m, func() tea.Msg {
				return kvSaveMsg{Entries: m.entries}
			}
		}
		return m, nil

	case key.Matches(msg, tui.KvEditorKeys.Back):
		if m.dirty {
			m.mode = kvModeConfirm
			m.confirmAction = kvConfirmDiscard
			m.confirmMessage = "Discard unsaved changes? (y/n)"
			return m, nil
		}
		return m, func() tea.Msg { return kvCancelMsg{} }

	case key.Matches(msg, tui.KvEditorKeys.Quit):
		return m, tea.Quit
	}

	return m, nil
}

func (m KVEditorModel) updateEditOverlay(msg tea.KeyPressMsg) (KVEditorModel, tea.Cmd) {
	// If cursor is on the custom value item, forward typing to textinput
	if m.overlayCursor < len(m.overlayChoices) &&
		m.overlayChoices[m.overlayCursor].choice == kvChoiceCustom {
		if !key.Matches(msg, tui.KvOverlayKeys.Up, tui.KvOverlayKeys.Down, tui.KvOverlayKeys.Confirm, tui.KvOverlayKeys.Cancel) {
			var cmd tea.Cmd
			m.overlayInput, cmd = m.overlayInput.Update(msg)
			return m, cmd
		}
	}

	switch {
	case key.Matches(msg, tui.KvOverlayKeys.Up):
		if m.overlayCursor > 0 {
			m.overlayCursor--
			m.updateOverlayInputFocus()
		}
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.Down):
		if m.overlayCursor < len(m.overlayChoices)-1 {
			m.overlayCursor++
			m.updateOverlayInputFocus()
		}
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.Confirm):
		if m.overlayCursor >= 0 && m.overlayCursor < len(m.overlayChoices) {
			selected := m.overlayChoices[m.overlayCursor]

			// Handle ignore toggle — calls API directly, no value change
			if selected.choice == kvChoiceIgnore {
				if m.overlayEntry != nil && !m.overlayEntry.Ignored {
					entryKey := m.overlayEntry.Key
					entrySection := m.overlayEntry.Section
					m.mode = kvModeList
					m.overlayEntry = nil
					return m, func() tea.Msg { return kvIgnoreMsg{Key: entryKey, Section: entrySection} }
				}
				// Already ignored — just close overlay
				m.mode = kvModeList
				m.overlayEntry = nil
				return m, nil
			}

			var newValue string
			switch selected.choice {
			case kvChoiceCustom:
				newValue = m.overlayInput.Value()
				if newValue == "" {
					return m, nil // don't allow empty custom value
				}
			default:
				newValue = selected.value
			}

			if m.overlayEntry != nil {
				wasIgnored := m.overlayEntry.Ignored
				entryKey := m.overlayEntry.Key
				entrySection := m.overlayEntry.Section

				// Find the entry in the list and update it
				for i := range m.entries {
					if m.entries[i].Key == entryKey {
						if m.entries[i].Value != newValue {
							m.entries[i].Value = newValue
							m.entries[i].Warnings = nil // clear old warnings
							m.dirty = true
						}
						break
					}
				}

				// If entry was ignored and user picked a real value, un-ignore it
				if wasIgnored {
					m.mode = kvModeList
					m.overlayEntry = nil
					return m, func() tea.Msg { return kvIgnoreMsg{Key: entryKey, Section: entrySection} }
				}
			}
		}
		m.mode = kvModeList
		m.overlayEntry = nil
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.Cancel):
		m.mode = kvModeList
		m.overlayEntry = nil
		return m, nil
	}

	return m, nil
}

// addFocusField identifies which field is focused in the add overlay.
type addFocusField int

const (
	addFocusSection addFocusField = iota
	addFocusKeyField
	addFocusValueField
)

func (m KVEditorModel) updateAddOverlay(msg tea.KeyPressMsg) (KVEditorModel, tea.Cmd) {
	focus := m.addFocusTarget()

	switch {
	// Navigation: Tab/Shift+Tab and Up/Down move between fields
	case key.Matches(msg, tui.KvOverlayKeys.Tab) || key.Matches(msg, tui.KvOverlayKeys.Down):
		m.advanceAddFocus(true)
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.ShiftTab) || key.Matches(msg, tui.KvOverlayKeys.Up):
		m.advanceAddFocus(false)
		return m, nil

	// Left/Right on Type field toggles parameter/environment
	case focus == addFocusSection && (key.Matches(msg, tui.KvOverlayKeys.Left) || key.Matches(msg, tui.KvOverlayKeys.Right)):
		m.toggleAddSection()
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.Confirm):
		k := strings.TrimSpace(m.addKeyInput.Value())
		v := strings.TrimSpace(m.addValueInput.Value())
		if k == "" {
			return m, nil
		}
		// Force uppercase for environment variables
		if m.addSection == kvSectionEnvironment {
			k = strings.ToUpper(k)
		}
		// Check for duplicate key within same section
		for _, e := range m.entries {
			if e.Key == k && e.Section == m.addSection {
				m.statusMsg = fmt.Sprintf("Key %q already exists", k)
				m.statusError = true
				return m, nil
			}
		}
		m.entries = append(m.entries, KVEntry{
			Key:     k,
			Value:   v,
			Section: m.addSection,
			Source:  "custom",
		})
		m.dirty = true
		m.mode = kvModeList
		m.cursor = len(m.entries) - 1
		return m, nil

	case key.Matches(msg, tui.KvOverlayKeys.Cancel):
		m.mode = kvModeList
		return m, nil

	default:
		// Forward to active textinput
		if focus == addFocusKeyField {
			var cmd tea.Cmd
			m.addKeyInput, cmd = m.addKeyInput.Update(msg)
			// Auto-uppercase for environment variables as user types
			if m.addSection == kvSectionEnvironment {
				m.addKeyInput.SetValue(strings.ToUpper(m.addKeyInput.Value()))
			}
			return m, cmd
		}
		if focus == addFocusValueField {
			var cmd tea.Cmd
			m.addValueInput, cmd = m.addValueInput.Update(msg)
			return m, cmd
		}
		return m, nil
	}
}

// toggleAddSection switches between parameter and environment variable types.
func (m *KVEditorModel) toggleAddSection() {
	if m.addSection == kvSectionParameter {
		m.addSection = kvSectionEnvironment
		m.addKeyInput.Placeholder = "ENV_VAR_NAME"
		m.addKeyInput.Validate = validateEnvKey
		// Uppercase existing key input
		if v := m.addKeyInput.Value(); v != "" {
			m.addKeyInput.SetValue(strings.ToUpper(v))
		}
	} else {
		m.addSection = kvSectionParameter
		m.addKeyInput.Placeholder = "parameter-name"
		m.addKeyInput.Validate = validateParamKey
	}
}

// validateParamKey allows only valid CLI parameter key characters: a-z, 0-9, -, _
func validateParamKey(s string) error {
	for _, r := range s {
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isLower && !isUpper && !isDigit && r != '-' && r != '_' {
			return fmt.Errorf("invalid character: %c", r)
		}
	}
	return nil
}

// validateEnvKey allows only valid environment variable characters: A-Z, 0-9, _
func validateEnvKey(s string) error {
	for _, r := range s {
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isUpper && !isDigit && r != '_' {
			return fmt.Errorf("invalid character: %c", r)
		}
	}
	return nil
}

// validateValue allows printable characters only (no control chars).
func validateValue(s string) error {
	for _, r := range s {
		if unicode.IsControl(r) && r != '\t' {
			return fmt.Errorf("invalid character")
		}
	}
	return nil
}

// addFocusTarget returns the current focus field in the add overlay.
func (m KVEditorModel) addFocusTarget() addFocusField {
	if !m.addKeyInput.Focused() && !m.addValueInput.Focused() {
		return addFocusSection
	}
	if m.addFocusKey {
		return addFocusKeyField
	}
	return addFocusValueField
}

// advanceAddFocus cycles focus: section → key → value → section.
func (m *KVEditorModel) advanceAddFocus(forward bool) {
	current := m.addFocusTarget()
	var next addFocusField
	if forward {
		switch current {
		case addFocusSection:
			next = addFocusKeyField
		case addFocusKeyField:
			next = addFocusValueField
		case addFocusValueField:
			next = addFocusSection
		}
	} else {
		switch current {
		case addFocusSection:
			next = addFocusValueField
		case addFocusKeyField:
			next = addFocusSection
		case addFocusValueField:
			next = addFocusKeyField
		}
	}
	m.setAddFocus(next)
}

func (m *KVEditorModel) setAddFocus(f addFocusField) {
	switch f {
	case addFocusSection:
		m.addFocusKey = false
		m.addKeyInput.Blur()
		m.addValueInput.Blur()
	case addFocusKeyField:
		m.addFocusKey = true
		m.addKeyInput.Focus()
		m.addValueInput.Blur()
	case addFocusValueField:
		m.addFocusKey = false
		m.addKeyInput.Blur()
		m.addValueInput.Focus()
	}
}

func (m KVEditorModel) updateConfirm(msg tea.KeyPressMsg) (KVEditorModel, tea.Cmd) {
	switch {
	case key.Matches(msg, tui.DefaultConfirmKeyBindings.Yes):
		action := m.confirmAction
		idx := m.confirmIndex
		m.mode = kvModeList
		m.confirmMessage = ""
		m.confirmAction = ""

		switch action {
		case kvConfirmDelete:
			if idx >= 0 && idx < len(m.entries) {
				deletedKey := m.entries[idx].Key
				deletedSection := m.entries[idx].Section
				m.entries = append(m.entries[:idx], m.entries[idx+1:]...)
				if m.cursor >= len(m.entries) && len(m.entries) > 0 {
					m.cursor = len(m.entries) - 1
				}
				m.dirty = true
				return m, func() tea.Msg { return kvDeleteMsg{Key: deletedKey, Section: deletedSection} }
			}
		case kvConfirmDiscard:
			return m, func() tea.Msg { return kvCancelMsg{} }
		}
		return m, nil

	case key.Matches(msg, tui.DefaultConfirmKeyBindings.No, tui.DefaultConfirmKeyBindings.Cancel):
		m.mode = kvModeList
		m.confirmMessage = ""
		m.confirmAction = ""
		return m, nil
	}

	return m, nil
}

// ============================================================================
// Overlay helpers
// ============================================================================

// setFullBackground allows the parent view to inject the full-screen background
// (including breadcrumb, title bar, etc.) for overlay dimming.
func (m *KVEditorModel) setFullBackground(bg string) {
	m.cachedListBg = bg
}

func (m *KVEditorModel) openEditOverlay(entry *KVEntry) {
	if m.cachedListBg == "" {
		m.cachedListBg = m.viewList(m.width, m.height)
	}
	m.mode = kvModeEditValue
	m.overlayEntry = entry
	m.overlayChoices = m.buildRadioChoices(entry)
	m.overlayCursor = m.findCurrentChoice(entry)

	// Pre-fill textinput with current value for custom choice
	m.overlayInput.SetValue(entry.Value)
	m.overlayInput.SetWidth(40)
	m.updateOverlayInputFocus()
}

func (m *KVEditorModel) openAddOverlay() {
	if m.cachedListBg == "" {
		m.cachedListBg = m.viewList(m.width, m.height)
	}
	m.mode = kvModeAddNew
	m.addKeyInput.SetValue("")
	m.addValueInput.SetValue("")
	m.addSection = kvSectionParameter
	m.addKeyInput.SetWidth(40)
	m.addValueInput.SetWidth(40)
	m.addKeyInput.Placeholder = "parameter-name"
	m.setAddFocus(addFocusSection)
}

func (m *KVEditorModel) updateOverlayInputFocus() {
	if m.overlayCursor < len(m.overlayChoices) &&
		m.overlayChoices[m.overlayCursor].choice == kvChoiceCustom {
		m.overlayInput.Focus()
	} else {
		m.overlayInput.Blur()
	}
}

func (m *KVEditorModel) buildRadioChoices(entry *KVEntry) []kvRadioItem {
	var choices []kvRadioItem

	// 1. Default option (model mode only)
	if m.modelMode && entry.DefaultValue != "" {
		label := fmt.Sprintf("default (%s", entry.DefaultValue)
		if entry.DefaultSource != "" {
			label += ", from " + entry.DefaultSource
		}
		label += ")"
		choices = append(choices, kvRadioItem{
			label:  label,
			choice: kvChoiceDefault,
			value:  entry.DefaultValue,
		})
	}

	// 2. Auto option
	if entry.SupportsAuto {
		choices = append(choices, kvRadioItem{
			label:  "auto",
			choice: kvChoiceAuto,
			value:  "auto",
		})
	}

	// 3. Predefined options
	for _, opt := range entry.Options {
		// Skip if already covered by auto
		if opt == "auto" && entry.SupportsAuto {
			continue
		}
		// Skip if same as default (already shown)
		if m.modelMode && opt == entry.DefaultValue {
			continue
		}
		choices = append(choices, kvRadioItem{
			label:  opt,
			choice: kvChoiceOption,
			value:  opt,
		})
	}

	// 4. Ignore option
	choices = append(choices, kvRadioItem{
		label:  "ignore (won't be passed at launch)",
		choice: kvChoiceIgnore,
		value:  "",
	})

	// 5. Custom value (always last)
	choices = append(choices, kvRadioItem{
		label:  "value:",
		choice: kvChoiceCustom,
		value:  "", // actual value comes from textinput
	})

	return choices
}

func (m *KVEditorModel) findCurrentChoice(entry *KVEntry) int {
	// If ignored, select the ignore choice
	if entry.Ignored {
		for i, c := range m.overlayChoices {
			if c.choice == kvChoiceIgnore {
				return i
			}
		}
	}

	val := entry.Value

	for i, c := range m.overlayChoices {
		switch c.choice {
		case kvChoiceCustom, kvChoiceIgnore:
			continue // checked separately
		default:
			if c.value == val {
				return i
			}
		}
	}

	// Fall back to custom value choice
	for i, c := range m.overlayChoices {
		if c.choice == kvChoiceCustom {
			return i
		}
	}
	return 0
}

// kvEditorOverhead is the lines used by: title(1) + blank(1) + header+sep(2) + footer(3).
const kvEditorOverhead = 7

func (m KVEditorModel) VisibleRows() int {
	rows := m.height - kvEditorOverhead
	if m.mode == kvModeConfirm {
		rows -= shared.TuiConfirmLines
	}
	if rows < 3 {
		return 3
	}
	return rows
}

// keyColumnWidth returns a proportional key column width based on terminal width.
func (m KVEditorModel) keyColumnWidth() int {
	w := shared.ProportionalWidth(m.width, 1, 3, 20)
	if w > 44 {
		return 44
	}
	return w
}

func (m KVEditorModel) handleMouseClick(msg tea.MouseClickMsg) (KVEditorModel, tea.Cmd) {
	if msg.Button == tea.MouseRight {
		if m.mode != kvModeList {
			m.mode = kvModeList
		} else {
			return m, func() tea.Msg { return kvCancelMsg{} }
		}
		return m, nil
	}
	// Content starts after: header(1) + separator(1) = 2 (app mode)
	// or title(1) + header(1) + separator(1) = 3 (model mode)
	headerLines := 2
	if m.modelMode {
		headerLines = 3
	}
	row := msg.Y - headerLines + m.offset
	if row >= 0 && row < len(m.entries) {
		if m.cursor == row && !m.entries[row].ReadOnly {
			m.openEditOverlay(&m.entries[row])
		} else {
			m.cursor = row
			m.statusMsg = ""
		}
	}
	return m, nil
}

func (m KVEditorModel) handleMouseWheel(msg tea.MouseWheelMsg) (KVEditorModel, tea.Cmd) {
	shared.MouseWheel(msg, &m.cursor, &m.offset, len(m.entries), m.VisibleRows())
	return m, nil
}

// ============================================================================
// View
// ============================================================================

func (m KVEditorModel) View(width, height int) string {
	if m.mode == kvModeEditValue {
		return m.viewEditOverlay(width, height)
	}
	if m.mode == kvModeAddNew {
		return m.viewAddOverlay(width, height)
	}
	return m.viewList(width, height)
}

func (m KVEditorModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	// Show title in model mode (app mode has tabs instead)
	if m.modelMode && m.title != "" {
		b.WriteString(m.styles.Title.Render(m.title))
		if m.dirty {
			b.WriteString(m.styles.Error.Render(" [modified]"))
		}
		b.WriteString("\n")
	} else if m.dirty {
		b.WriteString(" " + m.styles.Error.Render("[modified]") + "\n")
	}

	if len(m.entries) == 0 {
		b.WriteString(" No parameters\n")
	} else {
		// Confirmation dialog
		if m.mode == kvModeConfirm {
			b.WriteString(" " + m.styles.Error.Render(m.confirmMessage) + "\n\n")
		}

		vis := m.VisibleRows()
		end := min(m.offset+vis, len(m.entries))

		// Header — two columns: KEY and VALUE, styled as a single line for alignment
		keyW := m.keyColumnWidth()
		header := fmt.Sprintf("    %-*s %s", keyW, "KEY", "VALUE")
		b.WriteString(m.styles.Help.Render(header) + "\n")
		b.WriteString(" " + shared.Hrule(m.styles, width-2) + "\n")

		maxValW := max(
			// prefix(4) + gaps
			width-keyW-8, 10)

		// Track whether we've shown the environment section header
		envHeaderShown := false

		for i := m.offset; i < end; i++ {
			e := m.entries[i]

			// Show section separator before first environment variable
			if e.Section == kvSectionEnvironment && !envHeaderShown {
				envHeaderShown = true
				if i > 0 {
					b.WriteString("\n")
					b.WriteString("    " + m.styles.Help.Render("ENVIRONMENT VARIABLES") + "\n")
					b.WriteString(" " + shared.Hrule(m.styles, width-2) + "\n")
				}
			}

			prefix := "    "
			if i == m.cursor {
				prefix = " \u25b8  " // ▸ with padding
			}

			keyStr := shared.TruncateText(e.Key, keyW-2)

			// Value with resolved hint and indicators
			valStr := e.Value
			if e.AutoResolvedHint != "" && e.AutoResolvedHint != e.Value {
				valStr = valStr + " (" + e.AutoResolvedHint + ")"
			}
			if len(e.Warnings) > 0 {
				valStr = valStr + " !"
			} else if e.Ignored {
				valStr = valStr + " [ignored]"
			}
			valStr = shared.TruncateText(valStr, maxValW)

			line := fmt.Sprintf("%s%-*s %s",
				prefix, keyW, keyStr, valStr)

			if i == m.cursor {
				b.WriteString(m.styles.Selected.Render(line) + "\n")
			} else if e.Ignored {
				b.WriteString(m.styles.Help.Render(line) + "\n")
			} else if len(e.Warnings) > 0 {
				b.WriteString(m.warningStyle.Render(line) + "\n")
			} else {
				b.WriteString(m.styles.Normal.Render(line) + "\n")
			}
		}
	}

	// Status + footer
	status := ""
	if m.statusMsg != "" {
		if m.statusError {
			status = m.styles.Error.Render(m.statusMsg)
		} else {
			status = m.statusMsg
		}
	}
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width, status, tui.KvEditorKeys.HintsString(m.dirty)))

	return b.String()
}

func (m KVEditorModel) viewEditOverlay(width, height int) string {
	entry := m.overlayEntry
	if entry == nil {
		return m.viewList(width, height)
	}

	// Build overlay content
	var popup strings.Builder

	// Title: key name
	title := entry.Key
	if entry.Label != "" {
		title = entry.Label + " (" + entry.Key + ")"
	}
	popup.WriteString(m.styles.Title.Render(title) + "\n")

	// Description
	if entry.Description != "" {
		popup.WriteString("\n")
		desc := entry.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		popup.WriteString(m.styles.Help.Render(desc) + "\n")
	}
	popup.WriteString("\n")

	// Radio choices
	for i, c := range m.overlayChoices {
		radio := "\u25cb" // ○
		if i == m.overlayCursor {
			radio = "\u25cf" // ●
		}

		var line string
		if c.choice == kvChoiceCustom {
			line = fmt.Sprintf("%s value: %s", radio, m.overlayInput.View())
		} else {
			line = fmt.Sprintf("%s %s", radio, c.label)
		}

		if i == m.overlayCursor {
			popup.WriteString(m.styles.Selected.Render(line) + "\n")
		} else {
			popup.WriteString(m.styles.Normal.Render(line) + "\n")
		}
	}

	popup.WriteString("\n" + m.styles.Help.Render(tui.KvOverlayKeys.EditHintsString()))

	return m.renderOverlay(popup.String(), width, height)
}

func (m KVEditorModel) viewAddOverlay(width, height int) string {
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render("Add Entry") + "\n\n")

	focus := m.addFocusTarget()

	// Section selector (parameter vs environment variable)
	sectionPrefix := "  "
	if focus == addFocusSection {
		sectionPrefix = "\u25b8 "
	}
	paramLabel := "parameter"
	envLabel := "environment variable"
	if m.addSection == kvSectionParameter {
		paramLabel = "[" + paramLabel + "]"
	} else {
		envLabel = "[" + envLabel + "]"
	}
	popup.WriteString(m.styles.Normal.Render(fmt.Sprintf("%sType:  %s  %s", sectionPrefix, paramLabel, envLabel)) + "\n")

	keyLabel := "  Key:   "
	valLabel := "  Value: "
	switch focus {
	case addFocusKeyField:
		keyLabel = "\u25b8 Key:   "
	case addFocusValueField:
		valLabel = "\u25b8 Value: "
	}

	popup.WriteString(m.styles.Normal.Render(keyLabel) + m.addKeyInput.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(valLabel) + m.addValueInput.View() + "\n")

	popup.WriteString("\n" + m.styles.Help.Render(tui.KvOverlayKeys.AddHintsString()))

	return m.renderOverlay(popup.String(), width, height)
}

func (m KVEditorModel) renderOverlay(content string, width, height int) string {
	// Calculate popup dimensions — base on terminal width, not fixed pixels
	popupWidth := min(width*3/4, 56)
	contentWidth := lipgloss.Width(content) + 6 // +6 for border(2) + padding(4)
	if contentWidth > popupWidth {
		popupWidth = contentWidth
	}
	if popupWidth > width-4 && width > 10 {
		popupWidth = width - 4
	}
	if popupWidth < 40 {
		popupWidth = 40
	}

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.styles.Theme.Primary).
		Padding(1, 2).
		Width(popupWidth)

	box := boxStyle.Render(content)

	w := width
	h := height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	// Use cached list render as background (captured when overlay opened)
	bg := m.cachedListBg
	if bg == "" {
		bg = m.viewList(w, h)
	}

	// Dim the background for visual focus on the overlay
	bg = dimContent(bg)

	// Use lipgloss v2 Layer/Compositor to overlay
	boxH := lipgloss.Height(box)
	boxW := lipgloss.Width(box)
	startCol := (w - boxW) / 2
	startRow := (h - boxH) / 2
	if startCol < 0 {
		startCol = 0
	}
	if startRow < 0 {
		startRow = 0
	}

	bgLayer := lipgloss.NewLayer(bg)
	fgLayer := lipgloss.NewLayer(box).X(startCol).Y(startRow).Z(1)
	comp := lipgloss.NewCompositor(bgLayer, fgLayer)
	canvas := lipgloss.NewCanvas(w, h)
	return canvas.Compose(comp).Render()
}

// dimContent replaces all visible text with a uniform dim color,
// preserving layout but removing color variation to fade the background.
func dimContent(s string) string {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("#444444"))
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		// Strip existing ANSI codes and re-render in dim
		plain := stripANSI(line)
		lines[i] = dim.Render(plain)
	}
	return strings.Join(lines, "\n")
}

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// Skip until 'm' (SGR terminator)
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
