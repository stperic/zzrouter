package views

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui/form"
)

// --- Team form (new / edit) ---

// intValueOrEmpty formats a positive int as a string, or "" for zero.
func intValueOrEmpty(n int) string {
	if n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

func (m *TeamsViewModel) openNewTeamForm() {
	m.mode = teamsViewNewTeam
	m.teamID = ""
	m.fName.SetValue("")
	m.fRPM.SetValue("")
	m.fTPM.SetValue("")
	m.fMaxParallel.SetValue("")
	m.fBudgetMax.SetValue("")
	m.fBudgetPeriod.SetSelected(0)
	m.fModels.SetSelected(nil)
	m.teamForm.FocusAt(0)
	m.statusMsg = ""
}

func (m *TeamsViewModel) openEditTeamForm(t *pkgClient.TeamResponse) {
	m.mode = teamsViewEditTeam
	m.teamID = t.ID
	m.fName.SetValue(t.Name)
	m.fRPM.SetValue(intValueOrEmpty(t.RPMLimit))
	m.fTPM.SetValue(intValueOrEmpty(t.TPMLimit))
	m.fMaxParallel.SetValue(intValueOrEmpty(t.MaxParallelRequests))
	if t.SpendLimit > 0 {
		m.fBudgetMax.SetValue(fmt.Sprintf("%.2f", t.SpendLimit))
	} else {
		m.fBudgetMax.SetValue("")
	}
	m.fBudgetPeriod.SetSelectedByValue(t.ResetPeriod)
	m.fModels.SetSelected(t.AllowedModels)
	m.teamForm.FocusAt(0)
	m.statusMsg = ""
}

// countTeamKeys returns how many loaded keys report the given TeamID.
// Used by the list/delete flows to show "N members" without hitting the
// /teams/:id/keys endpoint on every keystroke.
func countTeamKeys(keys []*pkgClient.KeyResponse, teamID string) int {
	n := 0
	for _, k := range keys {
		if k.TeamID == teamID {
			n++
		}
	}
	return n
}

// keysInTeam returns loaded keys whose TeamID matches, sorted by ID.
func keysInTeam(keys []*pkgClient.KeyResponse, teamID string) []*pkgClient.KeyResponse {
	out := make([]*pkgClient.KeyResponse, 0, len(keys))
	for _, k := range keys {
		if k.TeamID == teamID {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *TeamsViewModel) updateTeamForm(msg tea.KeyPressMsg) tea.Cmd {
	// View owns the submit / cancel shortcuts; everything else belongs to
	// the reusable form package.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return m.saveTeamForm()

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		if m.mode == teamsViewEditTeam {
			m.mode = teamsViewDetail
		} else {
			m.mode = teamsViewList
		}
		m.statusMsg = ""
		return nil
	}

	cmd := m.teamForm.Update(msg)
	return cmd
}

func (m *TeamsViewModel) saveTeamForm() tea.Cmd {
	name := strings.TrimSpace(m.fName.Value())
	if name == "" {
		m.statusMsg = "Name is required"
		return nil
	}

	// New teams derive their ID (URL slug) from the name. Edit mode keeps
	// the existing ID captured at openEditTeamForm time.
	var id string
	if m.mode == teamsViewNewTeam {
		id = slugify(name)
		if id == "" {
			m.statusMsg = "Name must contain letters or digits"
			return nil
		}
	} else {
		id = m.teamID
	}

	rpm, _ := strconv.Atoi(strings.TrimSpace(m.fRPM.Value()))
	tpm, _ := strconv.Atoi(strings.TrimSpace(m.fTPM.Value()))
	maxPar, _ := strconv.Atoi(strings.TrimSpace(m.fMaxParallel.Value()))

	var spendLimit float64
	var resetPeriod string
	budgetMaxStr := strings.TrimSpace(m.fBudgetMax.Value())
	if budgetMaxStr != "" {
		parsed, err := strconv.ParseFloat(budgetMaxStr, 64)
		if err != nil {
			m.statusMsg = "Invalid budget amount"
			return nil
		}
		spendLimit = parsed
		resetPeriod = m.fBudgetPeriod.Value()
		if resetPeriod == "" {
			resetPeriod = "monthly"
		}
	}

	allowedModels := m.fModels.Selected()

	client := m.client

	if m.mode == teamsViewNewTeam {
		req := &pkgClient.CreateTeamRequest{
			ID:                  id,
			Name:                name,
			AllowedModels:       allowedModels,
			MaxParallelRequests: maxPar,
			RPMLimit:            rpm,
			TPMLimit:            tpm,
			SpendLimit:          spendLimit,
			ResetPeriod:         resetPeriod,
		}
		return func() tea.Msg {
			team, err := client.CreateTeam(req)
			return teamsSavedMsg{team: team, err: err}
		}
	}

	// Edit mode — PATCH with partial update. Membership is key-owned now;
	// this form doesn't touch it.
	teamID := id
	req := &pkgClient.UpdateTeamRequest{
		Name:                &name,
		MaxParallelRequests: &maxPar,
		RPMLimit:            &rpm,
		TPMLimit:            &tpm,
		SpendLimit:          &spendLimit,
		ResetPeriod:         &resetPeriod,
	}
	// AllowedModels — always send so clearing the picker actually clears state.
	if allowedModels == nil {
		empty := []string{}
		req.AllowedModels = &empty
	} else {
		req.AllowedModels = &allowedModels
	}

	return func() tea.Msg {
		team, err := client.UpdateTeam(teamID, req)
		return teamsSavedMsg{team: team, err: err}
	}
}

// --- Overlay rendering ---

func (m *TeamsViewModel) viewTeamFormOverlay(width, height int, title string) string {
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render(title) + "\n")

	// Slug preview — live for new teams, static for edits. Attached to the
	// Name field as a trailing hint so we don't spend a whole extra line.
	slug := m.teamID
	if m.mode == teamsViewNewTeam {
		slug = slugify(m.fName.Value())
		if slug == "" {
			slug = "auto from name"
		}
	}
	m.fName.SetHint("slug: " + slug)

	popup.WriteString(form.Section("Identity", m.styles) + "\n")
	popup.WriteString(m.fName.View(0, m.styles) + "\n")

	popup.WriteString(form.Section("Limits", m.styles) + "\n")
	popup.WriteString(m.fRPM.View(0, m.styles) + "\n")
	popup.WriteString(m.fTPM.View(0, m.styles) + "\n")
	popup.WriteString(m.fMaxParallel.View(0, m.styles) + "\n")
	popup.WriteString(m.fBudgetMax.View(0, m.styles) + "\n")
	popup.WriteString(m.fBudgetPeriod.View(0, m.styles) + "\n")

	popup.WriteString(form.Section("Access", m.styles) + "\n")
	popup.WriteString(m.fModels.View(0, m.styles) + "\n")

	popup.WriteString(m.styles.Help.Render("Keys are managed from the Keys view."))

	if m.statusMsg != "" {
		popup.WriteString("\n" + m.styles.Error.Render(m.statusMsg))
	}

	popup.WriteString("\n\n" + m.styles.Help.Render("↑↓/Tab field   ←→ option   Space toggle   Ctrl+S save   Esc cancel"))

	bg := m.viewList(width, height)
	return form.RenderOverlay(popup.String(), bg, width, height, m.styles)
}
