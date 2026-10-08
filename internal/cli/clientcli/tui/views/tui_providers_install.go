package views

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
)

// ============================================================================
// Install Wizard — step-by-step provider installation
// ============================================================================

// Step status constants
const (
	stepPending  = "pending"
	stepRunning  = "running"
	stepDone     = "done"
	stepFailed   = "failed"
	stepVerified = "verified"
)

// ============================================================================
// Messages
// ============================================================================

// providersInstallPlanMsg carries the fetched install plan.
type providersInstallPlanMsg struct {
	plan *pkgClient.InstallPlanResponse
	err  error
}

// providersStepVerifyMsg carries the result of verifying a single step.
type providersStepVerifyMsg struct {
	step   int
	result *pkgClient.StepResultResponse
	err    error
}

// providersVerifyMsg carries the result of a provider connectivity check.
type providersVerifyMsg struct {
	result *pkgClient.VerifyProviderResponse
	err    error
}

// providersStepExecuteMsg carries the result of executing a single step.
type providersStepExecuteMsg struct {
	step   int
	result *pkgClient.StepResultResponse
	err    error
}

// providersAutoInstallDoneMsg signals automatic install completed.
// Emitted when the jobstream subscription reaches a terminal FrameMsg.
type providersAutoInstallDoneMsg struct {
	err error
}

// providersInstallJobSubscribedMsg carries the Row from a successful
// POST /install or /execute-step + Subscribe. Parks on the model so
// Update can route subsequent FrameMsgs and Cancel() can tear it down
// on view exit. stepIdx=-1 indicates a wizard-level Run All; >=0 is
// the 0-based step index of a single-step Enter.
type providersInstallJobSubscribedMsg struct {
	row     *jobstream.Row
	err     error
	next    tea.Cmd
	stepIdx int
}

// providersPreflightMsg carries the result of preflight checks.
type providersPreflightMsg struct {
	report *pkgClient.PreflightResponse
	err    error
}

// ============================================================================
// Key map
// ============================================================================

type installWizardKeyMap struct {
	Up         key.Binding
	Down       key.Binding
	Enter      key.Binding
	RunAll     key.Binding
	Verify     key.Binding
	CopyCmd    key.Binding
	CopyScript key.Binding
	Back       key.Binding
}

var installWizardKeys = installWizardKeyMap{
	Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "navigate")),
	Down:       key.NewBinding(key.WithKeys("down", "j")),
	Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "run selected")),
	RunAll:     key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("A", "run all")),
	Verify:     key.NewBinding(key.WithKeys("v", "V"), key.WithHelp("V", "verify")),
	CopyCmd:    key.NewBinding(key.WithKeys("c", "C"), key.WithHelp("C", "copy cmd")),
	CopyScript: key.NewBinding(key.WithKeys("x", "X"), key.WithHelp("X", "copy script (all steps)")),
	Back:       key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
}

func installWizardHints() string {
	return tui.JoinHints(
		tui.Hint(installWizardKeys.Up, "navigate"),
		tui.Hint(installWizardKeys.Enter, "run selected"),
		tui.Hint(installWizardKeys.RunAll, "run all"),
		tui.Hint(installWizardKeys.Verify, "verify"),
		tui.Hint(installWizardKeys.CopyCmd, "copy cmd"),
		tui.Hint(installWizardKeys.CopyScript, "copy script (all)"),
		tui.Hint(installWizardKeys.Back, "back"),
	)
}

// ============================================================================
// Fetch command
// ============================================================================

func fetchInstallPlan(client *pkgClient.Client, name, node string) tea.Cmd {
	return func() tea.Msg {
		plan, err := client.GetInstallPlan(name, node)
		return providersInstallPlanMsg{plan: plan, err: err}
	}
}

func fetchPreflight(client *pkgClient.Client, name, node string) tea.Cmd {
	return func() tea.Msg {
		report, err := client.GetPreflight(name, node)
		return providersPreflightMsg{report: report, err: err}
	}
}

// ============================================================================
// Preflight failure modal
// ============================================================================

type preflightKeyMap struct {
	Retry key.Binding
	Back  key.Binding
}

var preflightKeys = preflightKeyMap{
	Retry: key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("R", "retry")),
	Back:  key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("Esc", "back")),
}

func (m *ProvidersViewModel) updatePreflightFail(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, preflightKeys.Retry):
		m.loading = true
		m.actionStatus = ""
		return tea.Batch(
			fetchPreflight(m.client, m.installProvider, m.installNode),
			shared.SpinnerTickCmd(),
		)
	case key.Matches(msg, preflightKeys.Back):
		m.mode = providersViewList
		m.preflightReport = nil
		m.actionStatus = ""
	}
	return nil
}

// preflightTextIndent aligns a check's message and hint under the ✗ icon
// and its check name.
const preflightTextIndent = "       "

func (m *ProvidersViewModel) viewPreflightFail(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	report := m.preflightReport

	// Modal title
	provName := report.Provider
	if len(provName) > 0 {
		provName = strings.ToUpper(provName[:1]) + provName[1:]
	}

	boxW := max(width-4, 40)
	if boxW > 80 {
		boxW = 80
	}
	innerW := boxW - 4

	boxStyle := m.styles.Help

	b.WriteString("\n")
	// Top border
	titleText := fmt.Sprintf(" Preflight Failed: %s ", provName)
	titleW := len(titleText)
	rightBorder := max(boxW-2-titleW, 0)
	b.WriteString("  " + boxStyle.Render("┌"+titleText+strings.Repeat("─", rightBorder)+"┐") + "\n")

	// Empty line
	b.WriteString("  " + boxStyle.Render("│") + strings.Repeat(" ", boxW-2) + boxStyle.Render("│") + "\n")

	// Failed checks only
	for _, check := range report.Results {
		if check.Passed {
			continue
		}

		// Error line
		icon := m.styles.Error.Render("✗")
		checkName := m.styles.Error.Bold(true).Render(check.Check)
		line := fmt.Sprintf("  %s  %s", icon, checkName)
		lineW := lipgloss.Width(line)
		pad := max(innerW-lineW, 0)
		b.WriteString("  " + boxStyle.Render("│") + " " + line + strings.Repeat(" ", pad) + " " + boxStyle.Render("│") + "\n")

		// Message line (indented, wrapped to the box interior)
		for _, msgLine := range strings.Split(shared.WrapIndent(m.styles.Normal.Render(check.Message), preflightTextIndent, innerW), "\n") {
			msgPad := max(innerW-lipgloss.Width(msgLine), 0)
			b.WriteString("  " + boxStyle.Render("│") + " " + msgLine + strings.Repeat(" ", msgPad) + " " + boxStyle.Render("│") + "\n")
		}

		// Hint line (indented, dim)
		if check.Hint != "" {
			for hintLine := range strings.SplitSeq(check.Hint, "\n") {
				for _, hl := range strings.Split(shared.WrapIndent(m.styles.Help.Render(hintLine), preflightTextIndent, innerW), "\n") {
					hlPad := max(innerW-lipgloss.Width(hl), 0)
					b.WriteString("  " + boxStyle.Render("│") + " " + hl + strings.Repeat(" ", hlPad) + " " + boxStyle.Render("│") + "\n")
				}
			}
		}

		// Blank line between checks
		b.WriteString("  " + boxStyle.Render("│") + strings.Repeat(" ", boxW-2) + boxStyle.Render("│") + "\n")
	}

	// Bottom separator + hints
	b.WriteString("  " + boxStyle.Render("│"+strings.Repeat("─", boxW-2)+"│") + "\n")
	hints := tui.JoinHints(
		tui.Hint(preflightKeys.Retry, "retry"),
		tui.Hint(preflightKeys.Back, "back"),
	)
	hintsLine := "  " + m.styles.HintsBar.Render(hints)
	hintsW := lipgloss.Width(hintsLine)
	hintsPad := max(innerW-hintsW, 0)
	b.WriteString("  " + boxStyle.Render("│") + " " + hintsLine + strings.Repeat(" ", hintsPad) + " " + boxStyle.Render("│") + "\n")
	b.WriteString("  " + boxStyle.Render("└"+strings.Repeat("─", boxW-2)+"┘") + "\n")

	return b.String()
}

// ============================================================================
// Update — handles key presses in the install wizard
// ============================================================================

func (m *ProvidersViewModel) updateInstallWizard(msg tea.KeyPressMsg) tea.Cmd {
	if m.installPlan == nil {
		// Still loading
		if key.Matches(msg, installWizardKeys.Back) {
			m.mode = providersViewList
		}
		return nil
	}

	steps := m.installPlan.Steps

	switch {
	case key.Matches(msg, installWizardKeys.Back):
		// Check if any steps completed — if so, refresh the provider list
		hadActivity := false
		for _, s := range m.installStepStatus {
			if s == stepDone || s == stepVerified {
				hadActivity = true
				break
			}
		}
		// Esc out of the wizard mid-install: cancel the active job
		// subscription so the goroutine exits before we leave the view.
		// Niling the row here means the synthetic FrameMsg{Done:true}
		// that Next emits on channel-close lands after the guard in
		// tui_providers.go §jobstream.FrameMsg clears it — dropped
		// silently. That's intended.
		if m.installJobRow != nil {
			m.installJobRow.Cancel()
			m.installJobRow = nil
		}
		m.installInFlight = false
		m.installProgress = nil
		m.mode = providersViewList
		m.installPlan = nil
		m.installStepStatus = nil
		if hadActivity {
			m.loading = true
			m.actionStatus = ""
			client := m.client
			return tea.Batch(func() tea.Msg {
				apps, err := shared.FetchProviders(client, "", "", true)
				return providersLoadedMsg{providers: apps, err: err}
			}, shared.SpinnerTickCmd())
		}

	case key.Matches(msg, installWizardKeys.Up):
		if m.installStepCursor > 0 {
			m.installStepCursor--
		}

	case key.Matches(msg, installWizardKeys.Down):
		if m.installStepCursor < len(steps)-1 {
			m.installStepCursor++
		}

	case key.Matches(msg, installWizardKeys.Verify):
		// Verify the current step
		idx := m.installStepCursor
		if idx < len(steps) {
			stepNum := steps[idx].Number
			client := m.client
			name, node := m.installPlan.Provider, m.installNode
			plan := *m.installPlan
			return func() tea.Msg {
				result, err := client.VerifyInstallStep(name, node, stepNum, &plan)
				return providersStepVerifyMsg{step: idx, result: result, err: err}
			}
		}

	case key.Matches(msg, installWizardKeys.Enter):
		// Per-step Enter: POST /execute-step returns 202 + job_id; the
		// step runs on a coordinator goroutine and emits on the jobs
		// stream. Re-running a done/failed step is allowed; any prior
		// live row is cancelled so the new subscription replaces it.
		idx := m.installStepCursor
		if idx < len(steps) && m.installStepStatus[idx] != stepRunning {
			m.installStepStatus[idx] = stepRunning
			stepNum := steps[idx].Number
			client := m.client
			name, node := m.installPlan.Provider, m.installNode
			m.installInFlight = true
			m.installProgress = nil
			if m.installJobRow != nil {
				m.installJobRow.Cancel()
				m.installJobRow = nil
			}
			m.installJobStep = idx
			return tea.Batch(
				startExecuteStepJobCmd(m.Context(), client, name, node, stepNum, idx, *m.installPlan),
				shared.SpinnerTickCmd(),
			)
		}

	case key.Matches(msg, installWizardKeys.RunAll):
		// Run all — POST /install returns 202 + job_id; subscribe to the
		// job stream for per-step progress + terminal. Cancel any prior
		// row (user hit A twice); the new subscription replaces it.
		for i := range m.installStepStatus {
			if m.installStepStatus[i] == stepPending {
				m.installStepStatus[i] = stepRunning
			}
		}
		client := m.client
		name, node := m.installPlan.Provider, m.installNode
		m.installInFlight = true
		m.installProgress = nil
		if m.installJobRow != nil {
			m.installJobRow.Cancel()
			m.installJobRow = nil
		}
		m.installJobStep = -1
		return startInstallJobCmd(m.Context(), client, name, node, *m.installPlan)

	case key.Matches(msg, installWizardKeys.CopyCmd):
		idx := m.installStepCursor
		if idx < len(steps) && steps[idx].Command != "" {
			if err := clipboard.WriteAll(steps[idx].Command); err != nil {
				m.actionStatus = fmt.Sprintf("✗ Copy failed: %v", err)
			} else {
				m.actionStatus = fmt.Sprintf("✓ Step %d command copied to clipboard", steps[idx].Number)
			}
		}

	case key.Matches(msg, installWizardKeys.CopyScript):
		script := buildInstallScript(m.installPlan)
		if err := clipboard.WriteAll(script); err != nil {
			m.actionStatus = fmt.Sprintf("✗ Copy failed: %v", err)
		} else {
			m.actionStatus = fmt.Sprintf("✓ Full install script copied to clipboard (%d commands)", len(steps))
		}
	}

	return nil
}

// startInstallJobCmd POSTs /install and subscribes to the returned
// job_id in one shot. Returns a tea.Cmd whose message is
// providersInstallJobSubscribedMsg (on success, carries the Row + the
// first Next cmd to Batch). On POST error, the msg.err is set and the
// wizard surfaces the failure via the same provider..Done handler.
// stepIdx = -1 flags this subscription as a wizard-level Run All.
func startInstallJobCmd(ctx context.Context, client *pkgClient.Client, name, node string, plans ...pkgClient.InstallPlanResponse) tea.Cmd {
	return func() tea.Msg {
		var plan *pkgClient.InstallPlanResponse
		if len(plans) > 0 {
			plan = &plans[0]
		}
		resp, err := client.InstallProvider(name, node, plan)
		if err != nil {
			return providersInstallJobSubscribedMsg{err: err, stepIdx: -1}
		}
		if resp == nil || resp.JobID == "" {
			return providersInstallJobSubscribedMsg{err: fmt.Errorf("server returned no job_id"), stepIdx: -1}
		}
		row, next := jobstream.Subscribe(ctx, client, resp.JobID, resp.Node)
		return providersInstallJobSubscribedMsg{row: row, next: next, stepIdx: -1}
	}
}

// startExecuteStepJobCmd mirrors startInstallJobCmd for a single step.
// stepIdx is the 0-based wizard cursor index that triggered the call
// and flows through to the terminal FrameMsg handler so it can mark
// the right row in installStepStatus.
func startExecuteStepJobCmd(ctx context.Context, client *pkgClient.Client, name, node string, stepNum, stepIdx int, plans ...pkgClient.InstallPlanResponse) tea.Cmd {
	return func() tea.Msg {
		var plan *pkgClient.InstallPlanResponse
		if len(plans) > 0 {
			plan = &plans[0]
		}
		resp, err := client.ExecuteInstallStep(name, node, stepNum, plan)
		if err != nil {
			return providersInstallJobSubscribedMsg{err: err, stepIdx: stepIdx}
		}
		if resp == nil || resp.JobID == "" {
			return providersInstallJobSubscribedMsg{err: fmt.Errorf("server returned no job_id"), stepIdx: stepIdx}
		}
		row, next := jobstream.Subscribe(ctx, client, resp.JobID, resp.Node)
		return providersInstallJobSubscribedMsg{row: row, next: next, stepIdx: stepIdx}
	}
}

// humanBytes formats a byte count with IEC units (KiB/MiB/GiB).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// buildInstallScript generates a runnable shell script from the install plan.
func buildInstallScript(plan *pkgClient.InstallPlanResponse) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -e\n")
	b.WriteString(fmt.Sprintf("# zzRouter install script for %s\n\n", plan.Provider))
	for _, step := range plan.Steps {
		if step.Command == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("# Step %d: %s\n", step.Number, step.Description))
		b.WriteString(step.Command + "\n\n")
	}
	return b.String()
}

// ============================================================================
// View — renders the install wizard
// ============================================================================

func (m *ProvidersViewModel) viewInstallWizard(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	if m.installPlan == nil {
		b.WriteString("\n " + shared.SpinnerFrame(m.loadTick) + " Fetching install plan...\n")
		return b.String()
	}

	sep := shared.Hrule(m.styles, width)
	accentBar := m.styles.Accent.Render("│")

	// Section header: title left, progress right
	doneCount := installStepCount(m.installStepStatus, stepDone, stepVerified)
	total := len(m.installPlan.Steps)
	provName := m.installPlan.Provider
	if len(provName) > 0 {
		provName = strings.ToUpper(provName[:1]) + provName[1:]
	}
	title := m.styles.Accent.Render(fmt.Sprintf(" Install %s Provider", provName))
	var progress string
	if doneCount == total && total > 0 {
		progress = m.styles.Success.Render(fmt.Sprintf("%d of %d: done ✓", doneCount, total))
	} else {
		progress = m.styles.Help.Render(fmt.Sprintf("%d of %d completed", doneCount, total))
	}
	titleW := lipgloss.Width(title)
	progressW := lipgloss.Width(progress)
	gap := max(width-titleW-progressW-2, 1)
	b.WriteString(title + strings.Repeat(" ", gap) + progress + " \n")
	b.WriteString(sep + "\n")

	// Step list
	for i, step := range m.installPlan.Steps {
		status := stepPending
		if i < len(m.installStepStatus) {
			status = m.installStepStatus[i]
		}

		// Status icon (left-aligned)
		icon := installStepIcon(m.styles, status, m.loadTick)

		selected := i == m.installStepCursor

		// Left bar for selected step
		var margin string
		if selected {
			margin = " " + accentBar + " "
		} else {
			margin = "    "
		}

		// Step line: icon + number + description
		desc := step.Description
		if status == stepVerified {
			desc += m.styles.Help.Render(" (verified)")
		}

		stepLine := fmt.Sprintf("%s %d. %s", icon, step.Number, desc)
		if selected {
			b.WriteString(margin + m.styles.Normal.Bold(true).Render(stepLine) + "\n")
		} else {
			b.WriteString(margin + m.styles.Normal.Render(stepLine) + "\n")
		}

		// Command box for selected step
		if selected && step.Command != "" {
			renderCommandBox(&b, m.styles, step.Command, width, margin)
		}
	}

	// Status area
	statusMsg := installStatusLine(m.styles, m.actionStatus)
	b.WriteString(sep + "\n")
	// Live progress line from the jobstream FrameMsg pipeline — only shown while a
	// one-shot install is in flight so a stale snapshot doesn't linger.
	if m.installInFlight && m.installProgress != nil {
		p := m.installProgress
		spin := stepSpinnerFrames[m.loadTick%len(stepSpinnerFrames)]
		line := fmt.Sprintf(" %s [%d/%d] %s", spin, p.Step, p.TotalSteps, p.StepDesc)
		if p.BytesTotal > 0 {
			line += fmt.Sprintf(" (%d%%, %s / %s)", p.Percent,
				humanBytes(p.BytesDone), humanBytes(p.BytesTotal))
		}
		b.WriteString(m.styles.StatusDownloading.Render(line) + "\n")
	}
	if statusMsg != "" {
		b.WriteString(statusMsg + "\n")
	}

	b.WriteString(shared.RenderViewFooter(m.styles, width, installWizardHints()))

	return b.String()
}

// ASCII spinner frames for step execution (avoids ambiguous-width Unicode).
var stepSpinnerFrames = [4]string{"|", "/", "-", "\\"}

// installStepIcon returns the styled icon for a step status.
// Uses ASCII characters to guarantee 1-cell width and consistent alignment.
func installStepIcon(styles ui.Styles, status string, tick int) string {
	switch status {
	case stepDone:
		return styles.Success.Render("*")
	case stepFailed:
		return styles.Error.Render("x")
	case stepRunning:
		return styles.StatusDownloading.Render(stepSpinnerFrames[tick%len(stepSpinnerFrames)])
	case stepVerified:
		return styles.Success.Render("*")
	default:
		return styles.Help.Render("-")
	}
}

// installStepCount counts steps matching any of the given statuses.
func installStepCount(statuses []string, match ...string) int {
	n := 0
	for _, s := range statuses {
		if slices.Contains(match, s) {
			n++
		}
	}
	return n
}

// installStatusLine renders the status message with appropriate styling.
func installStatusLine(styles ui.Styles, status string) string {
	if status == "" {
		return ""
	}
	if strings.HasPrefix(status, "✓") {
		return " " + styles.Success.Render(status)
	}
	if strings.HasPrefix(status, "✗") {
		return " " + styles.Error.Render(status)
	}
	return " " + styles.Help.Render(status)
}

// renderCommandBox renders a command inside a box.
func renderCommandBox(b *strings.Builder, styles ui.Styles, cmd string, width int, margin string) {
	marginW := lipgloss.Width(margin)
	contentW := max(
		// 4 = border (2) + padding (2)
		width-marginW-4, 20)

	cmdDisplay := "$ " + cmd
	if lipgloss.Width(cmdDisplay) > contentW {
		cmdDisplay = cmdDisplay[:contentW-1] + "…"
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.Theme.Overlay).
		Width(contentW).
		PaddingLeft(1).PaddingRight(1).
		Render(cmdDisplay)

	for line := range strings.SplitSeq(box, "\n") {
		b.WriteString(margin + line + "\n")
	}
}

// ============================================================================
// API Key Input — enter API key for cloud providers
// ============================================================================

// currentEnvVar returns the env var name currently being prompted.
func (m *ProvidersViewModel) currentEnvVar() string {
	if m.keyEnvIndex < len(m.keyEnvVars) {
		return m.keyEnvVars[m.keyEnvIndex]
	}
	return ""
}

// isSensitiveVar returns true if the env var contains a secret that should be masked.
func isSensitiveVar(envVar string) bool {
	upper := strings.ToUpper(envVar)
	return strings.Contains(upper, "KEY") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN")
}

// newEnvVarInput creates a textinput configured for the given env var.
// Sensitive vars (keys, secrets, tokens) are masked with EchoPassword.
func newEnvVarInput(envVar string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 512
	if isSensitiveVar(envVar) {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '*'
	}
	// Use white text for both focused and blurred states
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	s.Blurred.Text = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	ti.SetStyles(s)
	return ti
}

func (m *ProvidersViewModel) updateEnterKey(msg tea.Msg) tea.Cmd {
	// Forward all messages to textinput (for cursor blink, etc.)
	var tiCmd tea.Cmd
	m.keyInput, tiCmd = m.keyInput.Update(msg)

	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return tiCmd
	}

	switch {
	case key.Matches(keyMsg, tui.ProvidersKeys.Back):
		if m.keyEnvIndex > 0 {
			m.keyEnvIndex--
			m.keyInput = newEnvVarInput(m.currentEnvVar())
			m.actionStatus = ""
			return m.keyInput.Focus()
		}
		m.mode = providersViewPickProvider
		m.actionStatus = ""
		m.keyInput.Blur()
		m.buildProviderPicker()
		return nil

	case key.Matches(keyMsg, tui.ProvidersKeys.Enter):
		value := strings.TrimSpace(m.keyInput.Value())
		if value == "" {
			return tiCmd
		}
		envVar := m.currentEnvVar()
		if err := pkgConfig.SetEnvVar(envVar, value); err != nil {
			m.actionStatus = fmt.Sprintf("✗ Failed to save %s: %v", envVar, err)
			m.mode = providersViewList
			m.keyInput.Blur()
			return nil
		}

		// More env vars to prompt?
		if m.keyEnvIndex+1 < len(m.keyEnvVars) {
			m.actionStatus = fmt.Sprintf("✓ %s saved", envVar)
			m.keyEnvIndex++
			m.keyInput = newEnvVarInput(m.currentEnvVar())
			return m.keyInput.Focus()
		}

		// All env vars saved — verify
		m.actionStatus = fmt.Sprintf("✓ Saved to %s: verifying...", pkgConfig.GetEnvFilePath())
		m.mode = providersViewList
		m.loading = true
		m.keyInput.Blur()
		client := m.client
		provider := m.installProvider
		return tea.Batch(func() tea.Msg {
			result, err := client.VerifyProvider(provider)
			return providersVerifyMsg{result: result, err: err}
		}, shared.SpinnerTickCmd())
	}

	return tiCmd
}

func (m *ProvidersViewModel) viewEnterKey(width, height int) string { //nolint:unparam // Render helpers share width and height with the view dispatcher.
	var b strings.Builder

	envVar := m.currentEnvVar()

	b.WriteString("\n")

	// Progress indicator for multi-var flows
	if len(m.keyEnvVars) > 1 {
		b.WriteString(fmt.Sprintf(" %s\n",
			m.styles.Normal.Render(fmt.Sprintf("Step %d of %d", m.keyEnvIndex+1, len(m.keyEnvVars)))))
	}

	b.WriteString(" " + m.styles.Normal.Render("This provider requires "+envVar+" to connect.") + "\n")
	b.WriteString("\n")
	d := shared.NewDetail(m.styles, width)
	d.Field("Config file", pkgConfig.GetEnvFilePath())

	// Show node info from client
	if m.client != nil {
		nodeDisplay := m.client.Name()
		if nodeDisplay != "" {
			nodeDisplay += " (" + m.client.BaseURL() + ")"
		} else {
			nodeDisplay = m.client.BaseURL()
		}
		d.Field("Node", nodeDisplay)
	}
	b.WriteString(d.String())

	b.WriteString("\n")
	b.WriteString(" " + m.styles.Help.Render("You can either:") + "\n")
	b.WriteString(" " + m.styles.Help.Render("  • Set "+envVar+" manually and restart the server") + "\n")
	b.WriteString(" " + m.styles.Help.Render("  • Paste the value below to save it now") + "\n")
	b.WriteString("\n")

	b.WriteString(fmt.Sprintf(" %s %s\n", m.styles.DetailKey.Render(envVar+":"), m.keyInput.View()))

	// Show action status (verification result, errors)
	if m.actionStatus != "" {
		b.WriteString("\n")
		maxWidth := max(width-4, 40)
		if strings.HasPrefix(m.actionStatus, "✓") {
			b.WriteString(" " + m.styles.Success.Width(maxWidth).Render(m.actionStatus) + "\n")
		} else {
			b.WriteString(" " + m.styles.Error.Width(maxWidth).Render(m.actionStatus) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(shared.RenderViewFooter(m.styles, width, tui.JoinHints(
		"Ctrl+V paste",
		tui.Hint(tui.ProvidersKeys.Enter, "save"),
		tui.Hint(tui.ProvidersKeys.Back, "cancel"),
	)))

	return b.String()
}
