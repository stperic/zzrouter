package views

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
)

// --- Key form (new / edit) ---

func (m *VkeysViewModel) openNewKeyForm() {
	m.mode = vkeysViewNewKey
	m.formID.SetValue("")
	m.formName.SetValue("")
	m.formRole = 0
	m.formRPM.SetValue("")
	m.formTPM.SetValue("")
	m.formMaxParallel.SetValue("")
	m.formBudgetMax.SetValue("")
	m.formBudgetPeriod = 0
	m.formExpires.SetValue("")
	m.formFocus = vkeysFieldID
	m.statusMsg = ""
	m.selectedGroups = make(map[string]bool)
	m.groupsCursor = 0
	m.formMetadata = make(map[string]string)
	m.setKeyFormFocus()
}

func (m *VkeysViewModel) openEditKeyForm(k *pkgClient.KeyResponse) {
	m.mode = vkeysViewEditKey
	m.formID.SetValue(k.ID)
	m.formName.SetValue(k.Name)
	m.formRole = 0
	if k.Role == "admin" {
		m.formRole = 1
	}
	if k.RPMLimit > 0 {
		m.formRPM.SetValue(strconv.Itoa(k.RPMLimit))
	} else {
		m.formRPM.SetValue("")
	}
	if k.TPMLimit > 0 {
		m.formTPM.SetValue(strconv.Itoa(k.TPMLimit))
	} else {
		m.formTPM.SetValue("")
	}
	if k.MaxParallelRequests > 0 {
		m.formMaxParallel.SetValue(strconv.Itoa(k.MaxParallelRequests))
	} else {
		m.formMaxParallel.SetValue("")
	}
	if k.SpendLimit > 0 {
		m.formBudgetMax.SetValue(fmt.Sprintf("%.2f", k.SpendLimit))
	} else {
		m.formBudgetMax.SetValue("")
	}
	m.formBudgetPeriod = 0
	for i, p := range vkeysBudgetPeriods {
		if p == k.ResetPeriod {
			m.formBudgetPeriod = i
			break
		}
	}
	if k.ExpiresAt != nil {
		exp := *k.ExpiresAt
		if len(exp) >= 10 {
			m.formExpires.SetValue(exp[:10])
		} else {
			m.formExpires.SetValue(exp)
		}
	} else {
		m.formExpires.SetValue("")
	}
	m.formMetadata = make(map[string]string)
	maps.Copy(m.formMetadata, k.Metadata)
	m.formFocus = vkeysFieldName
	m.statusMsg = ""
	m.setKeyFormFocus()
}

func (m *VkeysViewModel) setKeyFormFocus() {
	m.formID.Blur()
	m.formName.Blur()
	m.formRPM.Blur()
	m.formTPM.Blur()
	m.formMaxParallel.Blur()
	m.formBudgetMax.Blur()
	m.formExpires.Blur()

	switch m.formFocus {
	case vkeysFieldID:
		m.formID.Focus()
	case vkeysFieldName:
		m.formName.Focus()
	case vkeysFieldRPM:
		m.formRPM.Focus()
	case vkeysFieldTPM:
		m.formTPM.Focus()
	case vkeysFieldMaxParallel:
		m.formMaxParallel.Focus()
	case vkeysFieldBudgetMax:
		m.formBudgetMax.Focus()
	case vkeysFieldExpires:
		m.formExpires.Focus()
		// AllowedModels is a toggle list — no text focus
	}
}

func (m *VkeysViewModel) updateKeyForm(msg tea.KeyPressMsg) tea.Cmd { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("tab"))):
		switch m.formFocus {
		case vkeysFieldID:
			m.formFocus = vkeysFieldName
		case vkeysFieldName:
			m.formFocus = vkeysFieldRole
		case vkeysFieldRole:
			m.formFocus = vkeysFieldRPM
		case vkeysFieldRPM:
			m.formFocus = vkeysFieldTPM
		case vkeysFieldTPM:
			m.formFocus = vkeysFieldMaxParallel
		case vkeysFieldMaxParallel:
			m.formFocus = vkeysFieldBudgetMax
		case vkeysFieldBudgetMax:
			m.formFocus = vkeysFieldBudgetPeriod
		case vkeysFieldBudgetPeriod:
			m.formFocus = vkeysFieldExpires
		case vkeysFieldExpires:
			if m.mode == vkeysViewNewKey {
				m.formFocus = vkeysFieldID
			} else {
				m.formFocus = vkeysFieldName
			}
		}
		m.setKeyFormFocus()
		return nil

	case m.formFocus == vkeysFieldRole && (key.Matches(msg, key.NewBinding(key.WithKeys("left"))) || key.Matches(msg, key.NewBinding(key.WithKeys("right")))):
		m.formRole = 1 - m.formRole
		return nil

	case m.formFocus == vkeysFieldBudgetPeriod && (key.Matches(msg, key.NewBinding(key.WithKeys("left"))) || key.Matches(msg, key.NewBinding(key.WithKeys("right")))):
		if msg.String() == "left" && m.formBudgetPeriod > 0 {
			m.formBudgetPeriod--
		} else if msg.String() == "right" && m.formBudgetPeriod < len(vkeysBudgetPeriods)-1 {
			m.formBudgetPeriod++
		}
		return nil

	case key.Matches(msg, key.NewBinding(key.WithKeys("ctrl+s"))):
		return m.saveKeyForm()

	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		if m.mode == vkeysViewEditKey {
			m.mode = vkeysViewDetail
		} else {
			m.mode = vkeysViewList
		}
		m.statusMsg = ""
		return nil

	default:
		var cmd tea.Cmd
		switch m.formFocus {
		case vkeysFieldID:
			m.formID, cmd = m.formID.Update(msg)
		case vkeysFieldName:
			m.formName, cmd = m.formName.Update(msg)
		case vkeysFieldRPM:
			m.formRPM, cmd = m.formRPM.Update(msg)
		case vkeysFieldTPM:
			m.formTPM, cmd = m.formTPM.Update(msg)
		case vkeysFieldMaxParallel:
			m.formMaxParallel, cmd = m.formMaxParallel.Update(msg)
		case vkeysFieldBudgetMax:
			m.formBudgetMax, cmd = m.formBudgetMax.Update(msg)
		case vkeysFieldExpires:
			m.formExpires, cmd = m.formExpires.Update(msg)
		}
		return cmd
	}
}

func (m *VkeysViewModel) saveKeyForm() tea.Cmd {
	id := strings.TrimSpace(m.formID.Value())
	name := strings.TrimSpace(m.formName.Value())

	if m.mode == vkeysViewNewKey && id == "" {
		m.statusMsg = "Key ID is required"
		return nil
	}
	if name == "" {
		m.statusMsg = "Name is required"
		return nil
	}

	role := "user"
	if m.formRole == 1 {
		role = "admin"
	}

	rpm, _ := strconv.Atoi(strings.TrimSpace(m.formRPM.Value()))
	tpm, _ := strconv.Atoi(strings.TrimSpace(m.formTPM.Value()))
	maxPar, _ := strconv.Atoi(strings.TrimSpace(m.formMaxParallel.Value()))

	var spendLimit float64
	var resetPeriod string
	budgetMaxStr := strings.TrimSpace(m.formBudgetMax.Value())
	if budgetMaxStr != "" {
		parsed, err := strconv.ParseFloat(budgetMaxStr, 64)
		if err != nil {
			m.statusMsg = "Invalid budget amount"
			return nil
		}
		spendLimit = parsed
		resetPeriod = vkeysBudgetPeriods[m.formBudgetPeriod]
		if resetPeriod == "" {
			resetPeriod = "monthly"
		}
	}

	var expiresAt *string
	expStr := strings.TrimSpace(m.formExpires.Value())
	if expStr != "" {
		// Convert YYYY-MM-DD to RFC3339
		rfc := expStr + "T23:59:59Z"
		expiresAt = &rfc
	}

	client := m.client

	if m.mode == vkeysViewNewKey {
		var metadata map[string]string
		if len(m.formMetadata) > 0 {
			metadata = m.formMetadata
		}
		req := &pkgClient.CreateKeyRequest{
			ID:                  id,
			Name:                name,
			Role:                role,
			RPMLimit:            rpm,
			TPMLimit:            tpm,
			MaxParallelRequests: maxPar,
			SpendLimit:          spendLimit,
			ResetPeriod:         resetPeriod,
			ExpiresAt:           expiresAt,
			Metadata:            metadata,
		}
		return func() tea.Msg {
			resp, err := client.CreateKey(req)
			return vkeysCreatedMsg{resp: resp, err: err}
		}
	}

	// Edit mode — PATCH with partial update
	keyID := strings.TrimSpace(m.formID.Value())
	req := &pkgClient.UpdateKeyRequest{
		Name: &name,
		Role: &role,
	}
	if rpm > 0 {
		req.RPMLimit = &rpm
	}
	if tpm > 0 {
		req.TPMLimit = &tpm
	}
	if maxPar > 0 {
		req.MaxParallelRequests = &maxPar
	}
	// Always send spend limit + reset period so clearing the field in the UI
	// actually clears the server-side limit (rather than leaving the old value).
	req.SpendLimit = &spendLimit
	req.ResetPeriod = &resetPeriod
	req.ExpiresAt = expiresAt
	if len(m.formMetadata) > 0 {
		req.Metadata = m.formMetadata
	}

	return func() tea.Msg {
		_, err := client.UpdateKey(keyID, req)
		if err != nil {
			return vkeysCreatedMsg{err: err} // reuse msg type for error display
		}
		// Refresh the list
		keys, err := client.ListKeys()
		return vkeysLoadedMsg{keys: keys, err: err}
	}
}

// --- Overlay rendering ---

func (m *VkeysViewModel) viewKeyFormOverlay(width, height int, title string) string {
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render(title) + "\n\n")

	focusPrefix := func(f vkeysFormField) string {
		if m.formFocus == f {
			return "▸ "
		}
		return "  "
	}

	if m.mode == vkeysViewNewKey {
		popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldID)+"ID:              ") + m.formID.View() + "\n")
	} else {
		popup.WriteString(m.styles.Help.Render("  ID:              "+m.formID.Value()) + "\n")
	}
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldName)+"Name:            ") + m.formName.View() + "\n")

	// Role radio
	roleUser := "( ) user"
	roleAdmin := "( ) admin"
	if m.formRole == 0 {
		roleUser = "(●) user"
	} else {
		roleAdmin = "(●) admin"
	}
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldRole)+"Role:            ") + roleUser + "  " + roleAdmin + "\n")

	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldRPM)+"RPM Limit:       ") + m.formRPM.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldTPM)+"TPM Limit:       ") + m.formTPM.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldMaxParallel)+"Max Parallel:    ") + m.formMaxParallel.View() + "\n")
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldBudgetMax)+"Budget (USD):    ") + m.formBudgetMax.View() + "\n")

	// Budget period radio
	var periodParts []string
	for i, p := range vkeysBudgetPeriods {
		label := p
		if label == "" {
			label = "none"
		}
		if i == m.formBudgetPeriod {
			periodParts = append(periodParts, "(●) "+label)
		} else {
			periodParts = append(periodParts, "( ) "+label)
		}
	}
	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldBudgetPeriod)+"Budget Period:   ") + strings.Join(periodParts, "  ") + "\n")

	// Model access is controlled at the team level — no per-key picker.
	popup.WriteString(m.styles.Help.Render("  Allowed Models:  (set on the team: use the Teams view)") + "\n")

	popup.WriteString(m.styles.Normal.Render(focusPrefix(vkeysFieldExpires)+"Expires:         ") + m.formExpires.View() + "\n")

	// Metadata display
	if len(m.formMetadata) > 0 {
		popup.WriteString("\n" + m.styles.Help.Render("  Metadata") + "\n")
		for mk, mv := range m.formMetadata {
			popup.WriteString(m.styles.Normal.Render(fmt.Sprintf("    %s: %s", mk, mv)) + "\n")
		}
	}

	if m.statusMsg != "" {
		popup.WriteString("\n" + m.styles.Error.Render(m.statusMsg) + "\n")
	}

	popup.WriteString("\n" + m.styles.Help.Render("Tab next field  ←/→ toggle  Space select model  Ctrl+S save  Esc cancel"))

	bg := m.viewList(width, height)
	return renderFormOverlay(popup.String(), bg, width, height, m.styles)
}

// updateRawKey drives the one-shot secret overlay. Only Copy, Save and an
// explicit Dismiss are honored: the raw key is Argon2id-hashed server
// side and can never be re-read, so a stray keystroke must not close
// the only display of it.
func (m *VkeysViewModel) updateRawKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, tui.VkeysKeys.Copy):
		return m.copyRawKeyCmd()

	case key.Matches(msg, tui.VkeysKeys.SaveKey):
		return m.saveRawKeyCmd()

	case key.Matches(msg, tui.VkeysKeys.Dismiss):
		m.mode = vkeysViewList
		m.rawKeyValue = ""
		m.rawKeyFlash = ""
		m.loading = true
		return m.Init()
	}
	return nil
}

// clipboardWrite is the native OS clipboard write (pbcopy on macOS,
// xclip/xsel/wl-copy on Linux). Indirected for tests, and tried before
// the terminal escape because terminals refuse OSC 52 clipboard writes
// by default — the escape silently does nothing on a stock iTerm2,
// Ghostty or Terminal.app.
var clipboardWrite = clipboard.WriteAll

// copyRawKeyCmd writes the secret to the local OS clipboard. The result
// decides whether the terminal-escape fallback is worth trying, so the
// write happens in a command rather than inline: it forks a helper
// process and must not block the update loop.
func (m *VkeysViewModel) copyRawKeyCmd() tea.Cmd {
	secret := m.rawKeyValue
	if secret == "" {
		return nil
	}
	return func() tea.Msg {
		return vkeysCopiedMsg{err: clipboardWrite(secret)}
	}
}

// rawKeyFileName is the file a saved key lands in. One fixed name, not
// one per key: the file is a hand-off buffer the operator empties, and
// accumulating secrets in a cache directory is the opposite of the goal.
const rawKeyFileName = "last-created-key.txt"

// saveRawKeyCmd writes the secret to a 0600 file. The escape hatch when
// the terminal blocks OSC 52 and mouse selection is unavailable — losing
// an unrecoverable key to a clipboard policy is the worse outcome.
func (m *VkeysViewModel) saveRawKeyCmd() tea.Cmd {
	secret := m.rawKeyValue
	if secret == "" {
		return nil
	}
	return func() tea.Msg {
		base, err := os.UserCacheDir()
		if err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		dir := filepath.Join(base, "zzrouter")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		path := filepath.Join(dir, rawKeyFileName)
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
			return vkeysSavedKeyMsg{err: err}
		}
		return vkeysSavedKeyMsg{path: path}
	}
}

// rawKeyBoxWidth is the inner secret panel's target width. Keys are 47
// characters; the panel is sized to hold one on a single line so a mouse
// drag selects the key and nothing else.
const rawKeyBoxWidth = 52

// rawKeyBoxChrome is what the popup adds around that panel: 2 cells of
// padding and 1 border cell on each side.
const rawKeyBoxChrome = 6

func (m *VkeysViewModel) viewRawKeyOverlay(width, height int) string {
	var popup strings.Builder

	popup.WriteString(m.styles.Title.Render("Key Created Successfully") + "\n\n")

	// The secret sits alone in its own frame: it is the payload, and a
	// row containing nothing else is a row the mouse can select cleanly.
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(m.styles.Theme.Overlay).
		Foreground(m.styles.Theme.Accent).
		Bold(true).
		Padding(0, 1).
		Width(min(rawKeyBoxWidth, max(width-12, 20)))
	popup.WriteString(box.Render(m.rawKeyValue) + "\n\n")

	// One status line, never two: the standing warning is replaced by the
	// outcome once the operator acts, so the box does not grow a stack of
	// stale messages.
	if m.rawKeyFlash != "" {
		popup.WriteString(m.styles.Success.Render(m.rawKeyFlash) + "\n")
	} else {
		popup.WriteString(m.styles.Error.Render("This key cannot be shown again.") + "\n")
	}

	popup.WriteString("\n" + m.styles.Help.Render(tui.VkeysKeys.RawKeyHintsString()))

	bg := m.viewList(width, height)
	// Sized to the secret panel plus the popup's own padding and borders,
	// so the dialog frames the key instead of a screenful of empty space.
	boxWidth := min(rawKeyBoxWidth+rawKeyBoxChrome, max(width-4, 24))
	return renderOverlayAtWidth(popup.String(), bg, width, height, boxWidth, m.styles)
}
