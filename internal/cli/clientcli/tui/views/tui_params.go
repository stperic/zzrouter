package views

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

type paramsLoadedMsg struct {
	ctx           context.Context
	scope         pkgClient.ParameterScope
	schema        *pkgClient.ProviderSchemaResponse
	before        *pkgClient.ConfiguredParameters
	effective     *pkgClient.ProviderResolved
	node, warning string
	err           error
}

type paramsSavedMsg struct {
	ctx       context.Context
	err       error
	uncertain bool
}

type paramsScopeMsg struct {
	owner *ParamsViewModel
	scope pkgClient.ParameterScope
}

// ParamsViewModel owns the draft and requests for one configured parameter scope.
type ParamsViewModel struct {
	tui.ViewContext
	tui.Draft
	client                      *pkgClient.Client
	styles                      ui.Styles
	Provider, Model             string
	node                        string
	scope                       pkgClient.ParameterScope
	editor                      KVEditorModel
	schema                      *pkgClient.ProviderSchemaResponse
	before                      *pkgClient.ConfiguredParameters
	effective                   *pkgClient.ProviderResolved
	loadContext, writeContext   context.Context
	loadCancel, writeCancel     context.CancelFunc
	loading, pending, uncertain bool
	status                      string
	termWidth, termHeight       int
}

// NewParamsViewModel opens the selected provider/model scope. Node selects an override on that machine.
func NewParamsViewModel(client *pkgClient.Client, styles ui.Styles, provider, model, node string) *ParamsViewModel {
	m := &ParamsViewModel{client: client, styles: styles, Provider: provider, Model: model, node: node}
	m.scope = pkgClient.ParameterScope{Scope: "defaults"}
	if model != "" {
		m.scope.Scope, m.scope.Model = "model", model
	}
	if node != "" && node != constants.CloudNodeName {
		m.scope.Node = node
		if model != "" {
			m.scope.Scope = "node-model"
		} else {
			m.scope.Scope = "node"
		}
	}
	m.Draft.Discard = m.resetEditor
	return m
}

// Init implements tea.Model.
func (m *ParamsViewModel) Init() tea.Cmd { return m.Refresh() }

// NavigationBlocked keeps the write owner present until its bounded request completes.
func (m *ParamsViewModel) NavigationBlocked() bool { return m.pending }

// Refresh reloads configured values without silently replacing a draft.
func (m *ParamsViewModel) Refresh() tea.Cmd {
	if m.pending || m.Dirty {
		return nil
	}
	if m.loadCancel != nil {
		m.loadCancel()
	}
	ctx, cancel := context.WithCancel(m.Context())
	m.loadContext, m.loadCancel, m.loading = ctx, cancel, true
	client, provider, scope, node := m.client, m.Provider, m.scope, m.node
	return func() tea.Msg {
		msg := paramsLoadedMsg{ctx: ctx, scope: scope, node: node}
		msg.schema, msg.err = client.GetProviderSchemaContext(ctx, provider)
		if msg.err == nil {
			msg.before, msg.err = client.GetConfiguredParameters(ctx, provider, scope)
		}
		if msg.err == nil {
			var err error
			msg.effective, err = client.GetProviderResolvedContext(ctx, provider, scope.Node, scope.Model, scope.Endpoint)
			if err != nil {
				msg.warning = "Effective values unavailable. Configured overrides are still editable."
			}
		}
		if msg.node == "" {
			msg.node, _ = client.GetCoordinatorNodeNameContext(ctx)
		}
		return msg
	}
}

// Cancel releases in-flight work and forgets environment values.
func (m *ParamsViewModel) Cancel() {
	m.ViewContext.Cancel()
	if m.loadCancel != nil {
		m.loadCancel()
	}
	if m.writeCancel != nil {
		m.writeCancel()
	}
	m.before, m.effective = nil, nil
	m.editor = KVEditorModel{}
}

// Update implements tea.Model.
func (m *ParamsViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.Draft.Update(msg); handled {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth, m.termHeight = msg.Width, msg.Height-shared.TuiTitleBarLines-shared.TuiBreadcrumbLines
		return m, nil
	case paramsLoadedMsg:
		m.loaded(msg)
		return m, nil
	case paramsSavedMsg:
		return m, m.saved(msg)
	case paramsScopeMsg:
		if msg.owner != m || m.pending {
			return m, nil
		}
		if m.ConfirmNavigation(msg) {
			return m, nil
		}
		if m.scope != msg.scope {
			m.before, m.schema, m.effective = nil, nil, nil
			m.editor = KVEditorModel{}
		}
		m.scope, m.status = msg.scope, ""
		return m, m.Refresh()
	case tea.KeyPressMsg:
		if m.pending {
			return m, nil
		}
		if cmd, handled := m.updateScopeKey(msg); handled {
			return m, cmd
		}

	}
	if m.pending || m.loading || m.before == nil {
		return m, nil
	}
	cmd, action := m.editor.update(msg)
	changes := parameterChanges(m.before, m.editor.entries)
	m.Dirty = len(changes.Parameters)+len(changes.Environment) > 0 || m.editor.formDirty()
	switch action {
	case kvSave:
		return m, m.save()
	case kvBack:
		return m, tui.NavBack()
	}
	return m, cmd
}

func (m *ParamsViewModel) resetEditor() {
	if m.before == nil || m.schema == nil {
		return
	}
	m.editor = newKVEditor(parameterEntries(m.before, m.schema, m.effective), m.styles)
	m.Dirty = false
}

func (m *ParamsViewModel) save() tea.Cmd {
	if m.pending || m.uncertain || !m.Dirty {
		return nil
	}
	changes := parameterChanges(m.before, m.editor.entries)
	before, scope, schema, provider, client := m.before, m.scope, m.schema, m.Provider, m.client
	ctx, cancel := context.WithTimeout(m.Context(), constants.HTTPLongTimeout)
	m.writeContext, m.writeCancel, m.pending, m.status = ctx, cancel, true, "Saving settings..."
	return func() tea.Msg {
		fresh, err := client.GetConfiguredParameters(ctx, provider, scope)
		if err != nil {
			return paramsSavedMsg{ctx: ctx, err: err}
		}
		if conflicts := parameterConflicts(before, fresh, changes); len(conflicts) > 0 {
			return paramsSavedMsg{ctx: ctx, err: fmt.Errorf("settings changed in another session: %s. Reload before editing", strings.Join(conflicts, ", "))}
		}
		err = client.PatchConfiguredParameters(ctx, provider, scope, changes, schema)
		return paramsSavedMsg{ctx: ctx, err: err, uncertain: writeOutcomeUnknown(err)}
	}
}

func configuredFromEntries(scope pkgClient.ParameterScope, entries []KVEntry) *pkgClient.ConfiguredParameters {
	out := &pkgClient.ConfiguredParameters{ParameterScope: scope, Parameters: map[string]string{}, Environment: map[string]string{}}
	for _, e := range entries {
		if !e.Configured {
			continue
		}
		if e.Section == kvSectionEnvironment {
			out.Environment[e.Key] = e.Value
		} else {
			out.Parameters[e.Key] = e.Value
		}
	}
	return out
}

func parameterChanges(before *pkgClient.ConfiguredParameters, entries []KVEntry) pkgClient.ParameterChanges {
	changes := pkgClient.ParameterChanges{Parameters: map[string]*string{}, Environment: map[string]*string{}}
	for _, e := range entries {
		values, target := before.Parameters, changes.Parameters
		if e.Section == kvSectionEnvironment {
			values, target = before.Environment, changes.Environment
		}
		value, present := values[e.Key]
		if e.Configured && (!present || value != e.Value) {
			v := e.Value
			target[e.Key] = &v
		} else if !e.Configured && present {
			target[e.Key] = nil
		}
	}
	return changes
}

func parameterConflicts(before, fresh *pkgClient.ConfiguredParameters, changes pkgClient.ParameterChanges) []string {
	var conflicts []string
	for group, changed := range map[string]map[string]*string{kvSectionParameter: changes.Parameters, kvSectionEnvironment: changes.Environment} {
		old, current := before.Parameters, fresh.Parameters
		if group == kvSectionEnvironment {
			old, current = before.Environment, fresh.Environment
		}
		for key := range changed {
			a, existsA := old[key]
			b, existsB := current[key]
			if existsA != existsB || a != b {
				conflicts = append(conflicts, group+"."+key)
			}
		}
	}
	slices.Sort(conflicts)
	return conflicts
}

func parameterEntries(before *pkgClient.ConfiguredParameters, schema *pkgClient.ProviderSchemaResponse, effective *pkgClient.ProviderResolved) []KVEntry {
	if effective == nil {
		effective = &pkgClient.ProviderResolved{}
	}
	var entries []KVEntry
	for _, group := range []string{kvSectionParameter, kvSectionEnvironment} {
		configured, shapes, evidence := before.Parameters, schema.Parameters, effective.Parameters
		if group == kvSectionEnvironment {
			configured, shapes, evidence = before.Environment, schema.Environment, effective.Environment
		}
		keys := map[string]bool{}
		for key := range configured {
			keys[key] = true
		}
		for key := range shapes {
			keys[key] = true
		}
		for key := range evidence {
			keys[key] = true
		}
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			value, present := configured[key]
			shape := shapes[key]
			e := KVEntry{Key: key, Section: group, Value: value, Configured: present, Description: shape.Description, Options: slices.Clone(shape.Enum)}
			if shape.Type == "bool" && len(e.Options) == 0 {
				e.Options = []string{"true", "false"}
			}
			if shape.Auto != "" && !slices.Contains(e.Options, shape.Auto) {
				e.Options = append(e.Options, shape.Auto)
			}
			if resolved, ok := evidence[key]; ok {
				e.Effective, e.Source = fmt.Sprint(resolved.Value), resolved.Tier
			}
			entries = append(entries, e)
		}
	}
	return entries
}

// Breadcrumb implements tui.Breadcrumber.
func (m *ParamsViewModel) Breadcrumb() []string { return []string{"Menu", "Settings", m.Provider} }

// View implements tea.Model.
func (m *ParamsViewModel) View() tea.View {
	label := "Provider defaults"
	if m.Model != "" {
		label = "Model: " + m.Model
	}
	if m.scope.Node != "" {
		label += " on " + m.scope.Node
	}
	label += "  | Tab scope  R reload"
	body := "Loading settings..."
	if !m.loading {
		if m.before == nil {
			body = m.styles.Error.Render(m.status) + "\n" + shared.RenderViewFooter(m.styles, m.termWidth, "R retry  Esc back")
		} else {
			body = m.editor.View(m.termWidth, max(m.termHeight-1, 1), m.status, m.Dirty)
		}
	}
	body = shared.TruncateText(label, m.termWidth) + "\n" + body
	body = m.Draft.View(m.styles, body, m.termWidth, m.termHeight)
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), body)
}

func (m *ParamsViewModel) loaded(msg paramsLoadedMsg) {

	if msg.ctx == nil || msg.ctx != m.loadContext || msg.ctx.Err() != nil || m.Context().Err() != nil || m.scope != msg.scope {
		return
	}
	m.loading = false
	if msg.err != nil {
		m.status = "Could not reload settings: " + msg.err.Error()
		return
	}
	m.before, m.schema, m.effective, m.node = msg.before, msg.schema, msg.effective, msg.node
	m.uncertain = false
	m.resetEditor()
	if msg.warning != "" {
		m.status = msg.warning
	}
}

func (m *ParamsViewModel) saved(msg paramsSavedMsg) tea.Cmd {

	if msg.ctx != m.writeContext || m.Context().Err() != nil {
		return nil
	}
	m.pending = false
	m.writeCancel()
	m.writeContext = nil
	if msg.err != nil {
		m.uncertain = msg.uncertain
		m.status = "Save failed: " + msg.err.Error()
		if m.uncertain {
			m.status = "Save outcome unknown. R reload before saving again. " + msg.err.Error()
		}
		return nil
	}
	m.Dirty = false
	m.status = "Saved. Changes apply on the next start."
	// Mark the submitted draft as the new baseline even if the reread fails.
	m.before = configuredFromEntries(m.scope, m.editor.entries)
	return m.Refresh()
}

func (m *ParamsViewModel) updateScopeKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.editor.editing {
		switch msg.String() {
		case "tab":
			if m.node == "" || m.node == constants.CloudNodeName {
				m.status = "Node identity unavailable. R reload or use the console."
				return nil, true
			}
			scope := pkgClient.ParameterScope{Scope: "defaults", Model: m.Model}
			if m.Model != "" {
				scope.Scope = "model"
			}
			if m.scope.Node == "" {
				scope.Node = m.node
				if m.Model != "" {
					scope.Scope = "node-model"
				} else {
					scope.Scope = "node"
				}
			}
			return func() tea.Msg { return paramsScopeMsg{owner: m, scope: scope} }, true
		case "r", "R":
			scope := m.scope
			return func() tea.Msg { return paramsScopeMsg{owner: m, scope: scope} }, true
		case "esc":
			return tui.NavBack(), true
		}
	}
	return nil, false
}
