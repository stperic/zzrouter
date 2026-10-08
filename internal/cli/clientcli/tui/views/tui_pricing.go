package views

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
)

// pricingViewMode tracks what the pricing view is showing.
type pricingViewMode int

const (
	pricingViewList     pricingViewMode = iota // Operator overrides
	pricingViewUnpriced                        // Models that served traffic with no price
	pricingViewForm                            // Overlay: new / edit override
)

// wildcardModel mirrors pricing.WildcardModel. Repeated rather than
// imported: TUI views talk to the server through the client package and
// must not reach into server-side packages.
const wildcardModel = "*"

// pricingLoadedMsg carries the override list and the un-priced tally,
// fetched together so the header count is never stale against the table.
type pricingLoadedMsg struct {
	overrides []pkgClient.PricingOverride
	status    *pkgClient.PricingStatus
	err       error
}

// pricingSavedMsg carries the result of an upsert or a delete.
type pricingSavedMsg struct{ err error }

// pricingFormField identifies which field is focused in the override form.
type pricingFormField int

const (
	pricingFieldProvider pricingFormField = iota
	pricingFieldModel
	pricingFieldInput
	pricingFieldOutput
	pricingFieldCached
	pricingFieldNote
)

// unpricedRow is one entry of the un-priced tally, split back into its
// provider and model parts for prefilling the form.
type unpricedRow struct {
	provider string
	model    string
	requests int64
}

// overrideItem wraps PricingOverride to satisfy tui.Item.
type overrideItem struct{ pkgClient.PricingOverride }

func (o overrideItem) Title() string       { return o.Model }
func (o overrideItem) Description() string { return o.Provider }
func (o overrideItem) FilterValue() string { return o.Provider + " " + o.Model }

// unpricedItem wraps unpricedRow to satisfy tui.Item.
type unpricedItem struct{ unpricedRow }

func (u unpricedItem) Title() string       { return u.model }
func (u unpricedItem) Description() string { return u.provider }
func (u unpricedItem) FilterValue() string { return u.provider + " " + u.model }

type PricingViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	termWidth  int
	termHeight int
	loading    bool
	loadTick   int
	err        error
	mode       pricingViewMode

	overrides []pkgClient.PricingOverride
	unpriced  []unpricedRow
	list      tui.List
	unpriceds tui.List

	confirm        tui.Confirm
	confirmPending bool

	// Overlay form
	editing      bool // true = replacing an existing override
	formProvider textinput.Model
	formModel    textinput.Model
	formInput    textinput.Model
	formOutput   textinput.Model
	formCached   textinput.Model
	formNote     textinput.Model
	formFocus    pricingFormField

	statusMsg string
}

func NewPricingViewModel(client *pkgClient.Client, styles ui.Styles) *PricingViewModel {
	nav := tui.NavigationKeys{
		Up: tui.PricingKeys.Up, Down: tui.PricingKeys.Down,
		PageUp: tui.PricingKeys.PageUp, PageDown: tui.PricingKeys.PageDown,
		Home: tui.PricingKeys.Home, End: tui.PricingKeys.End,
	}
	list := tui.NewList()
	list.SetKeys(nav)
	unpriceds := tui.NewList()
	unpriceds.SetKeys(nav)

	return &PricingViewModel{
		client:       client,
		styles:       styles,
		loading:      true,
		list:         list,
		unpriceds:    unpriceds,
		confirm:      tui.NewConfirm(),
		formProvider: newFormInput("blank = any provider", 64),
		formModel:    newFormInput("model id, or * for all", 128),
		formInput:    newFormInput("USD per 1M input tokens", 16),
		formOutput:   newFormInput("USD per 1M output tokens", 16),
		formCached:   newFormInput("blank = same as input", 16),
		formNote:     newFormInput("why this rate", 128),
	}
}

func (m *PricingViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchCmd(), shared.SpinnerTickCmd())
}

func (m *PricingViewModel) fetchCmd() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		overrides, err := client.ListPricingOverrides()
		if err != nil {
			return pricingLoadedMsg{err: err}
		}
		// A status failure is not fatal: overrides are the primary
		// content and must render even when the tally is unavailable.
		status, _ := client.GetPricingStatus()
		return pricingLoadedMsg{overrides: overrides, status: status}
	}
}

func (m *PricingViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.unpriceds.SetVisible(vis)
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		m.mode = pricingViewList
		return m, nil

	case shared.SpinnerTickMsg:
		if m.loading {
			m.loadTick++
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case pricingLoadedMsg:
		m.loading = false
		m.err = msg.err
		m.overrides = msg.overrides
		m.syncItems(msg.status)
		return m, nil

	case pricingSavedMsg:
		if msg.err != nil {
			m.statusMsg = msg.err.Error()
			return m, nil
		}
		m.statusMsg = ""
		m.mode = pricingViewList
		m.loading = true
		return m, m.fetchCmd()

	case tea.KeyPressMsg:
		if m.confirmPending {
			return m, m.updateConfirm(msg)
		}
		if m.mode == pricingViewForm {
			return m, m.updateForm(msg)
		}
		return m, m.updateList(msg)
	}
	return m, nil
}

// syncItems refreshes both lists from the latest fetch. The un-priced
// tally arrives keyed "provider/model"; it is split back apart so a row
// can prefill the form.
func (m *PricingViewModel) syncItems(status *pkgClient.PricingStatus) {
	items := make([]tui.Item, len(m.overrides))
	for i, o := range m.overrides {
		items[i] = overrideItem{o}
	}
	m.list.SetItems(items)

	m.unpriced = nil
	if status != nil {
		for key, count := range status.Unpriced {
			provider, model := splitUnpricedKey(key)
			m.unpriced = append(m.unpriced, unpricedRow{provider: provider, model: model, requests: count})
		}
		// Most-hit first: the biggest un-priced spender is the one worth
		// pricing, not whichever one hashed first.
		sort.Slice(m.unpriced, func(i, j int) bool {
			if m.unpriced[i].requests != m.unpriced[j].requests {
				return m.unpriced[i].requests > m.unpriced[j].requests
			}
			return m.unpriced[i].model < m.unpriced[j].model
		})
	}
	uitems := make([]tui.Item, len(m.unpriced))
	for i, u := range m.unpriced {
		uitems[i] = unpricedItem{u}
	}
	m.unpriceds.SetItems(uitems)
}

// splitUnpricedKey undoes the "provider/model" join. Model ids contain
// slashes of their own ("anthropic/claude-…"), so only the first segment
// is the provider.
func splitUnpricedKey(key string) (provider, model string) {
	if i := strings.Index(key, "/"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func (m *PricingViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd {
	active := &m.list
	if m.mode == pricingViewUnpriced {
		active = &m.unpriceds
	}
	if active.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.PricingKeys.Unpriced):
		if m.mode == pricingViewUnpriced {
			m.mode = pricingViewList
		} else {
			m.mode = pricingViewUnpriced
		}
		return nil

	case key.Matches(msg, tui.PricingKeys.Enter):
		if m.mode == pricingViewUnpriced {
			if u := m.selectedUnpriced(); u != nil {
				m.openForm(pkgClient.PricingOverride{Provider: u.provider, Model: u.model}, false)
			}
			return nil
		}
		if o := m.selectedOverride(); o != nil {
			m.openForm(*o, true)
		}
		return nil

	case key.Matches(msg, tui.PricingKeys.New):
		m.openForm(pkgClient.PricingOverride{}, false)
		return nil

	case key.Matches(msg, tui.PricingKeys.Edit):
		if o := m.selectedOverride(); o != nil && m.mode == pricingViewList {
			m.openForm(*o, true)
		}
		return nil

	case key.Matches(msg, tui.PricingKeys.Delete):
		if m.mode == pricingViewList && m.selectedOverride() != nil {
			m.confirmPending = true
		}
		return nil

	case key.Matches(msg, tui.PricingKeys.Refresh):
		m.loading = true
		return tea.Batch(m.fetchCmd(), shared.SpinnerTickCmd())

	case key.Matches(msg, tui.PricingKeys.Back):
		if m.mode == pricingViewUnpriced {
			m.mode = pricingViewList
			return nil
		}
		return tui.NavBack()
	}
	return nil
}

func (m *PricingViewModel) updateConfirm(msg tea.KeyPressMsg) tea.Cmd {
	switch m.confirm.UpdateKey(msg) {
	case tui.ConfirmYes:
		m.confirmPending = false
		o := m.selectedOverride()
		if o == nil {
			return nil
		}
		client := m.client
		provider, model := o.Provider, o.Model
		return func() tea.Msg {
			return pricingSavedMsg{err: client.DeletePricingOverride(provider, model)}
		}
	case tui.ConfirmNo, tui.ConfirmCancelled:
		m.confirmPending = false
	}
	return nil
}

func (m *PricingViewModel) selectedOverride() *pkgClient.PricingOverride {
	i := m.list.Cursor()
	if i < 0 || i >= len(m.overrides) {
		return nil
	}
	return &m.overrides[i]
}

func (m *PricingViewModel) selectedUnpriced() *unpricedRow {
	i := m.unpriceds.Cursor()
	if i < 0 || i >= len(m.unpriced) {
		return nil
	}
	return &m.unpriced[i]
}

func (m *PricingViewModel) Breadcrumb() []string {
	title := "Pricing"
	switch m.mode {
	case pricingViewUnpriced:
		return []string{title, "Un-priced"}
	case pricingViewForm:
		if m.editing {
			return []string{title, "Edit Rate"}
		}
		return []string{title, "New Rate"}
	default:
		return []string{title}
	}
}

func (m *PricingViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *PricingViewModel) viewContent() string {
	switch m.mode {
	case pricingViewUnpriced:
		return m.viewUnpriced(m.termWidth, m.termHeight)
	case pricingViewForm:
		return m.viewFormOverlay(m.termWidth, m.termHeight)
	default:
		return m.viewList(m.termWidth, m.termHeight)
	}
}

// formatRate renders a per-million rate. Zero is not blank: it is the
// operator saying "not billed per token", which must be legible as a
// deliberate choice rather than a missing value.
func formatRate(perM float64) string {
	if perM == 0 {
		return "free"
	}
	return "$" + strconv.FormatFloat(perM, 'f', -1, 64)
}

func (m *PricingViewModel) viewList(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.overrides) == 0,
		EmptyText: m.emptyText(),
		BackHint:  tui.Hint(tui.ListKeys.Back, "back"),
		EmptyHint: tui.JoinHints(
			tui.Hint(tui.PricingKeys.New, "new rate"),
			tui.Hint(tui.PricingKeys.Unpriced, "un-priced"),
			tui.Hint(tui.ListKeys.Back, "back"),
		),
	}); done {
		return out
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	allRowStrs := make([][]string, len(m.overrides))
	for i, o := range m.overrides {
		model := o.Model
		if o.AppliesToAllModels {
			model = "* (all models)"
		}
		provider := o.Provider
		if provider == "" {
			provider = "any"
		}
		allRowStrs[i] = []string{
			model,
			provider,
			formatRate(o.InputCostPer1M),
			formatRate(o.OutputCostPer1M),
			o.Note,
		}
	}

	start, end := m.list.VisibleRange()
	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"MODEL", "PROVIDER", "IN / 1M", "OUT / 1M", "NOTE"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.list.Cursor(),
		Offset:    m.list.Offset(),
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   allRowStrs,
	})
	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}
	b.WriteString(t.Render() + "\n")

	if n := len(m.unpriced); n > 0 {
		b.WriteString(m.styles.Error.Render(
			fmt.Sprintf("  %d model(s) served traffic with no price. They settled at $0 and drew down no budget. Press U.", n)) + "\n")
	}
	if m.confirmPending {
		if o := m.selectedOverride(); o != nil {
			b.WriteString(m.styles.Error.Render(
				fmt.Sprintf("  Delete the rate for %q? Upstream pricing takes over again. (y/n)", o.Model)) + "\n")
		}
	}
	if m.statusMsg != "" {
		b.WriteString(m.styles.Error.Render("  "+m.statusMsg) + "\n")
	}

	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width,
		shared.TableStatus("Rates", len(m.overrides), m.list.Offset(), vis),
		tui.PricingKeys.ListHintsString()))
	return b.String()
}

// emptyText tells the operator which of the two empty states they are in:
// nothing to fix, or nothing fixed yet.
func (m *PricingViewModel) emptyText() string {
	if len(m.unpriced) > 0 {
		return fmt.Sprintf("No rate overrides yet, but %d model(s) served traffic with no price. Press U to review them.", len(m.unpriced))
	}
	return "No rate overrides. Upstream pricing is used for every model. Press N to override one."
}

func (m *PricingViewModel) viewUnpriced(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	if out, done := tui.RenderListPrelude(m.styles, width, tui.ListPrelude{
		Loading:   m.loading,
		LoadTick:  m.loadTick,
		Err:       m.err,
		Empty:     len(m.unpriced) == 0,
		EmptyText: "Every model that served traffic has a price. Nothing is settling at $0 by accident.",
		BackHint:  tui.Hint(tui.PricingKeys.Unpriced, "back to overrides"),
		EmptyHint: tui.JoinHints(tui.Hint(tui.PricingKeys.Unpriced, "back to overrides")),
	}); done {
		return out
	}

	var b strings.Builder
	vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))

	allRowStrs := make([][]string, len(m.unpriced))
	for i, u := range m.unpriced {
		provider := u.provider
		if provider == "" {
			provider = "any"
		}
		allRowStrs[i] = []string{u.model, provider, strconv.FormatInt(u.requests, 10)}
	}

	start, end := m.unpriceds.VisibleRange()
	t := shared.NewScrollTable(shared.ScrollTableConfig{
		Headers:   []string{"MODEL", "PROVIDER", "REQUESTS"},
		Width:     width,
		Vis:       vis,
		Cursor:    m.unpriceds.Cursor(),
		Offset:    m.unpriceds.Offset(),
		Styles:    m.styles,
		StatusCol: -1,
		AllRows:   allRowStrs,
	})
	for _, row := range allRowStrs[start:end] {
		t.Row(row...)
	}
	b.WriteString(t.Render() + "\n")
	b.WriteString(m.styles.Help.Render(
		"  These settled at $0. A budget cannot stop traffic it prices at nothing.") + "\n")
	b.WriteString(shared.RenderViewFooterWithStatus(m.styles, width,
		shared.TableStatus("Un-priced", len(m.unpriced), m.unpriceds.Offset(), vis),
		tui.PricingKeys.UnpricedHintsString()))
	return b.String()
}
