package views

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/pkg/ui/form"

	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// openForm loads an override into the form. editing distinguishes a
// correction to an existing rate from a new one; the (provider, model)
// pair is the identity either way, so both save through the same upsert.
func (m *PricingViewModel) openForm(o pkgClient.PricingOverride, editing bool) {
	m.Dirty = false
	m.Draft.Discard = func() { m.openForm(o, editing) }
	m.mode = pricingViewForm
	m.formDetail = tui.NewDetail()
	m.editing = editing
	m.statusMsg = ""

	m.formProvider.SetValue(o.Provider)
	m.formModel.SetValue(o.Model)
	m.formInput.SetValue(rateFieldValue(o.InputCostPer1M, editing))
	m.formOutput.SetValue(rateFieldValue(o.OutputCostPer1M, editing))
	m.formCached.SetValue(rateFieldValue(o.CacheReadCostPer1M, o.CacheReadCostPer1M > 0))
	m.formNote.SetValue(o.Note)

	m.formFocus = pricingFieldProvider
	if o.Provider != "" && o.Model == "" {
		m.formFocus = pricingFieldModel
	}
	m.setFormFocus()
}

// rateFieldValue renders a stored rate for editing. A new form starts
// blank so the placeholder shows; an existing override shows its real
// number, including a deliberate zero.
func rateFieldValue(perM float64, show bool) string {
	if !show {
		return ""
	}
	return strconv.FormatFloat(perM, 'f', -1, 64)
}

func (m *PricingViewModel) setFormFocus() {
	m.formProvider.Blur()
	m.formModel.Blur()
	m.formInput.Blur()
	m.formOutput.Blur()
	m.formCached.Blur()
	m.formNote.Blur()

	switch m.formFocus {
	case pricingFieldProvider:
		m.formProvider.Focus()
	case pricingFieldModel:
		m.formModel.Focus()
	case pricingFieldInput:
		m.formInput.Focus()
	case pricingFieldOutput:
		m.formOutput.Focus()
	case pricingFieldCached:
		m.formCached.Focus()
	case pricingFieldNote:
		m.formNote.Focus()
	}
}

func (m *PricingViewModel) updateForm(msg tea.KeyPressMsg) tea.Cmd {
	before := m.formValues()
	defer func() {
		if before != m.formValues() {
			m.Dirty = true
		}
	}()

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("tab", "down"))):
		m.formFocus = (m.formFocus + 1) % (pricingFieldNote + 1)
		m.setFormFocus()
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab", "up"))):
		m.formFocus = (m.formFocus + pricingFieldNote) % (pricingFieldNote + 1)
		m.setFormFocus()
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return m.saveForm()

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		return func() tea.Msg { return pricingCloseMsg{owner: m} }

	default:
		var cmd tea.Cmd
		switch m.formFocus {
		case pricingFieldProvider:
			m.formProvider, cmd = m.formProvider.Update(msg)
		case pricingFieldModel:
			m.formModel, cmd = m.formModel.Update(msg)
		case pricingFieldInput:
			m.formInput, cmd = m.formInput.Update(msg)
		case pricingFieldOutput:
			m.formOutput, cmd = m.formOutput.Update(msg)
		case pricingFieldCached:
			m.formCached, cmd = m.formCached.Update(msg)
		case pricingFieldNote:
			m.formNote, cmd = m.formNote.Update(msg)
		}
		return cmd
	}
}

// parseRate reads a per-million rate field. Blank means zero, which is a
// real value here, so the caller cannot distinguish blank from "0" — and
// does not need to: both mean "not billed on this axis".
func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

func (m *PricingViewModel) saveForm() tea.Cmd {
	if m.pending || m.writeUncertain {
		return nil
	}
	model := strings.TrimSpace(m.formModel.Value())
	provider := strings.TrimSpace(m.formProvider.Value())

	if model == "" {
		m.statusMsg = "Model is required (use * to cover every model on a provider)"
		return nil
	}
	if model == wildcardModel && provider == "" {
		m.statusMsg = "A wildcard needs a provider, otherwise it would price every model everywhere"
		return nil
	}

	in, err := parseRate(m.formInput.Value())
	if err != nil {
		m.statusMsg = "Input rate must be a number of dollars per 1M tokens"
		return nil
	}
	out, err := parseRate(m.formOutput.Value())
	if err != nil {
		m.statusMsg = "Output rate must be a number of dollars per 1M tokens"
		return nil
	}
	cached, err := parseRate(m.formCached.Value())
	if err != nil {
		m.statusMsg = "Cached rate must be a number of dollars per 1M tokens"
		return nil
	}
	if in < 0 || out < 0 || cached < 0 {
		m.statusMsg = "Rates cannot be negative"
		return nil
	}

	req := &pkgClient.PricingOverride{
		Provider:           provider,
		Model:              model,
		InputCostPer1M:     in,
		OutputCostPer1M:    out,
		CacheReadCostPer1M: cached,
		Note:               strings.TrimSpace(m.formNote.Value()),
	}
	client := m.client
	ctx := m.beginWrite()
	return func() tea.Msg {
		_, err := client.SetPricingOverrideContext(ctx, req)
		return pricingSavedMsg{ctx: ctx, err: err}
	}
}

func (m *PricingViewModel) viewFormOverlay(width, height int) string {
	var popup strings.Builder
	focusLine := 0
	for _, input := range []*textinput.Model{&m.formProvider, &m.formModel, &m.formInput, &m.formOutput, &m.formCached, &m.formNote} {
		input.SetWidth(max(form.OverlayWidth(width)-22, 1))
	}

	title := "New Rate"
	if m.editing {
		title = "Edit Rate"
	}
	popup.WriteString(m.styles.Title.Render(title) + "\n\n")

	focusPrefix := func(f pricingFormField) string {
		if m.formFocus == f {
			focusLine = strings.Count(popup.String(), "\n")
			return "> "
		}
		return "  "
	}

	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldProvider)+"Provider:     ") + m.formProvider.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldModel)+"Model:        ") + m.formModel.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldInput)+"Input $/1M:   ") + m.formInput.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldOutput)+"Output $/1M:  ") + m.formOutput.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldCached)+"Cached $/1M:  ") + m.formCached.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(pricingFieldNote)+"Note:         ") + m.formNote.View() + "\n")

	popup.WriteString("\n" + m.styles.Help.Render(
		"Rates are dollars per million tokens. Leave both at 0 for a provider") + "\n")
	popup.WriteString(m.styles.Help.Render(
		"that bills by subscription: its traffic stops counting as un-priced.") + "\n")

	if m.statusMsg != "" {
		popup.WriteString("\n" + m.styles.Error.Render(m.statusMsg) + "\n")
	}

	return tui.RenderForm(m.styles, &m.formDetail, m.viewList(width, height), popup.String(), "Ctrl+S save  Tab next  Esc cancel", width, height, focusLine)
}

func (m *PricingViewModel) formValues() [6]string {
	return [6]string{m.formProvider.Value(), m.formModel.Value(), m.formInput.Value(), m.formOutput.Value(), m.formCached.Value(), m.formNote.Value()}
}
