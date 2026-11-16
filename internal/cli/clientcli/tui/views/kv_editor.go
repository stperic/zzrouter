package views

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

const (
	kvSectionParameter   = "parameters"
	kvSectionEnvironment = "environment"
)

// KVEntry separates a configured override from informational effective evidence.
type KVEntry struct {
	Key, Section, Value            string
	Configured                     bool
	Effective, Source, Description string
	Options                        []string
}

func (e KVEntry) FilterValue() string { return e.Section + " " + e.Key }

// kvItem supplies the list's navigation identity without storing another draft.
type kvItem struct{ entry KVEntry }

func (i kvItem) Title() string       { return i.entry.Key }
func (i kvItem) Description() string { return i.entry.Description }
func (i kvItem) FilterValue() string { return i.entry.FilterValue() }

type kvAction int

const (
	kvNone kvAction = iota
	kvSave
	kvBack
	kvChanged
)

// KVEditorModel edits overrides locally; only the parent can submit an API write.
type KVEditorModel struct {
	entries              []KVEntry
	list                 tui.List
	styles               ui.Styles
	width, height        int
	editing, adding      bool
	field                int
	section              string
	keyInput, valueInput textinput.Model
	viewport             tui.Detail
}

func newKVEditor(entries []KVEntry, styles ui.Styles) KVEditorModel {
	m := KVEditorModel{entries: entries, styles: styles, list: tui.NewList(), viewport: tui.NewDetail(),
		keyInput: newFormInput("Variable name", 128), valueInput: newFormInput("Value", 0)}
	m.syncItems()
	return m
}

func (m *KVEditorModel) syncItems() {
	items := make([]tui.Item, len(m.entries))
	for i, entry := range m.entries {
		items[i] = kvItem{entry}
	}
	m.list.SetItems(items)
}

func (m *KVEditorModel) selected() *KVEntry {
	if i := m.list.Cursor(); i >= 0 && i < len(m.entries) {
		return &m.entries[i]
	}
	return nil
}

func (m *KVEditorModel) update(msg tea.Msg) (tea.Cmd, kvAction) {
	if m.editing {
		return m.updateForm(msg)
	}
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		m.list.UpdateWheel(msg)
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseRight {
			return nil, kvBack
		}
		if msg.Button == tea.MouseLeft {
			if enter, ok := m.list.Click(msg, shared.TuiTableClickY+1); ok && enter {
				m.openForm(false)
			}
		}
	case tea.KeyPressMsg:
		if m.list.UpdateKey(msg) {
			return nil, kvNone
		}
		switch msg.String() {
		case "enter", "e", "E":
			m.openForm(false)
		case "a", "A":
			m.openForm(true)
		case "d", "D":
			if entry := m.selected(); entry != nil && entry.Configured {
				entry.Configured, entry.Value = false, ""
				return nil, kvChanged
			}
		case "ctrl+s", "s", "S":
			return nil, kvSave
		case "esc":
			return nil, kvBack
		}
	}
	return nil, kvNone
}

func (m *KVEditorModel) openForm(add bool) {
	entry := m.selected()
	if !add && entry == nil {
		return
	}
	m.editing, m.adding, m.field = true, add, 1
	m.viewport.Reset()
	m.valueInput.EchoMode = textinput.EchoNormal
	if add {
		m.field, m.section = 0, kvSectionEnvironment
		m.keyInput.SetValue("")
		m.valueInput.SetValue("")
	} else {
		m.section = entry.Section
		m.keyInput.SetValue(entry.Key)
		m.valueInput.SetValue(entry.Value)
	}
	if m.section == kvSectionEnvironment {
		m.valueInput.EchoMode = textinput.EchoPassword
	}
	m.focusInput()
}

func (m *KVEditorModel) focusInput() {
	m.keyInput.Blur()
	m.valueInput.Blur()
	if m.field == 0 {
		m.keyInput.Focus()
	} else {
		m.valueInput.Focus()
	}
}

func (m *KVEditorModel) updateForm(msg tea.Msg) (tea.Cmd, kvAction) {
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.editing = false
			m.valueInput.SetValue("")
			return nil, kvNone
		case "tab", "shift+tab":
			if m.adding {
				m.field = 1 - m.field
				m.focusInput()
			}
			return nil, kvNone
		case "ctrl+t":
			if m.adding {
				if m.section == kvSectionEnvironment {
					m.section = kvSectionParameter
					m.valueInput.EchoMode = textinput.EchoNormal
				} else {
					m.section = kvSectionEnvironment
					m.valueInput.EchoMode = textinput.EchoPassword
				}
			}
			return nil, kvNone
		case "up", "down":
			if !m.adding {
				entry := m.selected()
				if len(entry.Options) > 0 {
					index := slices.Index(entry.Options, m.valueInput.Value())
					step := 1
					if key.String() == "up" {
						step = -1
					}
					index = (index + step + len(entry.Options)) % len(entry.Options)
					m.valueInput.SetValue(entry.Options[index])
				}
			}
			return nil, kvNone
		case "enter", "ctrl+s":
			name := strings.TrimSpace(m.keyInput.Value())
			if name == "" {
				return nil, kvNone
			}
			index := slices.IndexFunc(m.entries, func(e KVEntry) bool { return e.Key == name && e.Section == m.section })
			if index < 0 {
				m.entries = append(m.entries, KVEntry{Key: name, Section: m.section})
				index = len(m.entries) - 1
			}
			m.entries[index].Configured, m.entries[index].Value = true, m.valueInput.Value()
			m.syncItems()
			m.list.SetCursor(index)
			m.editing = false
			m.valueInput.SetValue("")
			return nil, kvChanged
		}
	}
	var cmd tea.Cmd
	if m.field == 0 {
		m.keyInput, cmd = m.keyInput.Update(msg)
	} else {
		m.valueInput, cmd = m.valueInput.Update(msg)
	}
	return cmd, kvNone
}

func (m *KVEditorModel) View(width, height int, status string, dirty bool) string {
	m.width, m.height = width, height
	hints := "Enter edit  A add  D clear override  Esc back"
	if dirty {
		hints = "Enter edit  Ctrl+S save  D clear  Esc back"
	}
	footer := shared.RenderViewFooterWithStatus(m.styles, width, status, hints)
	vis := shared.VisibleRows(height, footer)
	m.list.SetVisible(vis)
	rows := make([][]string, len(m.entries))
	for i, e := range m.entries {
		configured, effective := "Inherited", e.Effective
		if e.Configured {
			configured = e.Value
			if configured == "" {
				configured = "\"\""
			}
		}
		name := e.Key
		if e.Section == kvSectionEnvironment {
			name = "env:" + name
			if e.Configured {
				configured = "Hidden"
			}
			if effective != "" {
				effective = "Hidden"
			}
		}
		rows[i] = []string{name, configured, effective}
	}
	table := shared.NewScrollTable(shared.ScrollTableConfig{Headers: []string{"SETTING", "OVERRIDE", "EFFECTIVE"},
		Width: width, Vis: vis, Cursor: m.list.Cursor(), Offset: m.list.Offset(), Styles: m.styles, StatusCol: -1, AllRows: rows})
	start, end := m.list.VisibleRange()
	for _, row := range rows[start:end] {
		table.Row(row...)
	}
	body := table.Render() + "\n" + footer
	if !m.editing {
		return body
	}
	return m.viewForm(body, width, height)
}

func (m *KVEditorModel) viewForm(bg string, width, height int) string {
	inner := max(form.OverlayWidth(width)-6, 1)
	m.keyInput.SetWidth(max(inner-7, 1))
	m.valueInput.SetWidth(max(inner-7, 1))
	title, detail := m.keyInput.Value(), ""
	if m.adding {
		title = "Add override"
	} else if entry := m.selected(); entry != nil {
		detail = entry.Description
		if entry.Section != kvSectionEnvironment && entry.Effective != "" {
			detail += fmt.Sprintf("\nEffective: %s (%s)", entry.Effective, entry.Source)
		}
		if len(entry.Options) > 0 {
			detail += "\nOptions: " + strings.Join(entry.Options, ", ") + ". Up/Down choose."
		}
	}
	var content strings.Builder
	content.WriteString(m.styles.Title.Render(title) + "\n")
	if m.adding {
		content.WriteString("Type: " + m.section + " (Ctrl+T changes)\nName:  " + m.keyInput.View() + "\n")
	}
	focusLine := strings.Count(content.String(), "\n")
	if m.field == 0 {
		focusLine--
	}
	content.WriteString("Value: " + m.valueInput.View() + "\n")
	if detail != "" {
		content.WriteString("\n" + m.styles.Help.Render(detail))
	}
	return tui.RenderForm(m.styles, &m.viewport, bg, content.String(), "Enter apply  Esc cancel", width, height, focusLine)
}

// formDirty includes unapplied input when the host navigates away from this editor.
func (m *KVEditorModel) formDirty() bool {
	if !m.editing {
		return false
	}
	if m.adding {
		return m.keyInput.Value() != "" || m.valueInput.Value() != ""
	}
	entry := m.selected()
	return entry != nil && entry.Value != m.valueInput.Value()
}
