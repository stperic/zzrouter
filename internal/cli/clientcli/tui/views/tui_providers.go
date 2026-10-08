package views

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	logsclient "github.com/stperic/zzrouter/internal/client/logs"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
)

// providersViewMode tracks what the view is showing.
type providersViewMode int

const (
	providersViewList          providersViewMode = iota // Table list
	providersViewDetail                                 // Detail panel for selected app
	providersViewPickType                               // Picker: cloud or local
	providersViewPickProvider                           // Picker: select provider type to install
	providersViewPickNode                               // Picker: select node for install
	providersViewInstallSteps                           // Step-by-step install wizard
	providersViewPreflightFail                          // Modal: preflight check failures
	providersViewEnterKey                               // API key input for cloud providers
	providersViewConnectForm                            // Form: add a remote ollama daemon (connect instance)
	providersViewUpgrade                                // Prompt: upgrade target version
)

// ollamaConnectCatalogKey is the synthetic picker entry that routes into
// the connect form flow instead of the normal install wizard.
const ollamaConnectCatalogKey = "ollama-connect"

type providersLoadedMsg struct {
	providers []shared.ProviderInfo
	err       error
}

// providersDetailParamsMsg carries parameters fetched for the detail view.
type providersDetailParamsMsg struct {
	params []pkgClient.ParameterEntry
	err    error
}

// providersStatusMsg carries provider status fetched for the detail view.
type providersStatusMsg struct {
	status *pkgClient.ProviderStatusResponse
}

// providersCatalogMsg carries the provider catalog from provider config.
type providersCatalogMsg struct {
	catalog []pkgClient.ProviderCatalogEntry
	err     error
}

// providersVerifySuccessTimerMsg fires after showing verify success for 3 seconds.
type providersVerifySuccessTimerMsg struct{}

// providersActionMsg carries result of install/uninstall operations.
type providersActionMsg struct {
	action string // "install", "uninstall" or "upgrade"
	name   string
	err    error
}

// providerItem wraps shared.ProviderInfo to satisfy tui.Item.
type providerItem struct{ shared.ProviderInfo }

func (p providerItem) Title() string       { return p.Name }
func (p providerItem) Description() string { return p.Kind }
func (p providerItem) FilterValue() string { return p.Name + " " + p.Kind }

type ProvidersViewModel struct {
	tui.ViewContext

	client     *pkgClient.Client
	styles     ui.Styles
	providers  []shared.ProviderInfo
	list       tui.List
	detail     tui.Detail
	termWidth  int
	termHeight int
	loading    bool
	err        error
	mode       providersViewMode

	// Detail view — parameter list
	detailParams        []pkgClient.ParameterEntry
	detailParamsLoading bool
	detailParamsErr     error
	loadTick            int

	// Detail view — provider rate limit status
	providerStatus *pkgClient.ProviderStatusResponse

	// Detail view — upstream release report for the selected provider.
	versionsReport *pkgClient.ProviderVersionsResponse

	// In-flight upgrade, streamed so the status line tracks the real work
	// rather than reporting success when the 202 lands. Nil when idle.
	upgrade *upgradeJob

	// Provider catalog (all defined in provider config)
	catalog []pkgClient.ProviderCatalogEntry

	// Install/uninstall pickers
	pickerItems     []string // items for the current picker
	pickerKeys      []string // keys corresponding to picker items
	pickerCursor    int
	pickerTitle     string
	installType     string // "cloud" or "local"
	installProvider string // selected provider name for install
	installNode     string // selected node for install
	actionStatus    string // status message after install/uninstall
	// actionUnconfirmed marks actionStatus as describing a change the table
	// has not been refetched for yet. An upgrade announces success while
	// the rows on screen are still the pre-upgrade ones, so the two
	// disagree until the fetch lands; the line says so rather than leaving
	// the contradiction unexplained.
	actionUnconfirmed bool

	// API key/env var input
	keyEnvVars  []string        // env vars to prompt for (e.g., ["AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_API_KEY"])
	keyEnvIndex int             // current index into keyEnvVars
	keyInput    textinput.Model // text input with cursor and masking

	// Upgrade prompt state
	upgradeProvider string          // provider being upgraded
	upgradeNode     string          // node the upgrade targets
	upgradeCurrent  string          // installed version, shown as context
	upgradeLatest   string          // newest upstream release, "" when unknown
	upgradeStatus   string          // how installed compares to upstream
	upgradeInput    textinput.Model // free-text version override (blank = use pin)
	upgradePin      bool            // also persist the typed version as the config pin

	// Preflight state
	preflightReport *pkgClient.PreflightResponse

	// Install wizard state
	installPlan       *pkgClient.InstallPlanResponse
	installStepCursor int
	installStepStatus []string // per-.Step: "pending", "running", "done", "failed", "verified"

	// installInFlight is true while an install request is running — either
	// the run-all path (/install, async+SSE) or a single-step Enter (sync).
	// Gates the wizard's progress line so it only renders while active.
	installInFlight bool
	// installProgress is the latest progress snapshot shown on the wizard.
	// Run All populates it from jobstream FrameMsgs; per-step path leaves
	// it nil (per-step completion is driven by providersStepExecuteMsg).
	installProgress *pkgClient.InstallProgress
	// installJobRow is the jobstream row for the active install operation
	// — either a Run All or a single Enter-triggered step. nil while idle.
	// Cancelled when the user leaves the wizard or the view is popped.
	installJobRow *jobstream.Row
	// installJobStep is the step index (0-based) this row is tracking
	// for a per-step Enter; -1 means Run All (wizard-level terminal).
	installJobStep int

	// needsRefresh is set when install/uninstall activity occurred in a sub-view.
	// Checked on transition back to list mode to trigger a provider data refresh.
	needsRefresh bool

	// Connect form (add a remote Ollama daemon) — reached from the Local
	// provider picker via the synthetic `ollama-connect` catalog entry.
	connectInputs     [connectFieldCount]textinput.Model
	connectFocus      int
	connectSubmitting bool
	connectErr        string
}

func NewProvidersViewModel(client *pkgClient.Client, styles ui.Styles) *ProvidersViewModel {
	list := tui.NewList()
	list.SetKeys(tui.NavigationKeys{
		Up: tui.ProvidersKeys.Up, Down: tui.ProvidersKeys.Down,
		PageUp: tui.ProvidersKeys.PageUp, PageDown: tui.ProvidersKeys.PageDown,
		Home: tui.ProvidersKeys.Home, End: tui.ProvidersKeys.End,
	})
	detail := tui.NewDetail()
	detail.SetKeys(tui.NavigationKeys{
		Up: tui.ProvidersKeys.Up, Down: tui.ProvidersKeys.Down,
		PageUp: tui.ProvidersKeys.PageUp, PageDown: tui.ProvidersKeys.PageDown,
		Home: tui.ProvidersKeys.Home, End: tui.ProvidersKeys.End,
	})
	return &ProvidersViewModel{client: client, styles: styles, loading: true, list: list, detail: detail}
}

// needsTick reports whether anything on screen still changes over time.
//
// An upgrade keeps the list on screen, so neither loading nor a running
// install step is set; without the job clause the tick chain ended on its
// first tick and the status line froze on whatever step was last emitted —
// which is how a step that ran for over two minutes read as hung.
func (m *ProvidersViewModel) needsTick() bool {
	return m.loading || m.hasRunningStep() || m.upgrade != nil
}

// hasRunningStep returns true if any install step is currently executing.
func (m *ProvidersViewModel) hasRunningStep() bool {
	return slices.Contains(m.installStepStatus, stepRunning)
}

// Cancel satisfies tui.Canceller. Tears down the active install job
// subscription (if any) before the view's ViewContext.Cancel fires.
// Called by Root on view pop/replace and by tui.Root.TeardownAll on
// program exit.
//
// installJobRow.Cancel is explicit (not redundant) because it spawns
// the drain goroutine that lets the producer exit even if the channel
// buffer is full at the moment ctx cancellation races with a pending
// send. ViewContext.Cancel alone cancels the ctx but can't drain.
func (m *ProvidersViewModel) Cancel() {
	if m.installJobRow != nil {
		m.installJobRow.Cancel()
		m.installJobRow = nil
	}
	// An upgrade outlives the prompt that started it, so leaving the view
	// mid-upgrade must tear the subscription down with it.
	if m.upgrade != nil {
		m.upgrade.row.Cancel()
		m.upgrade = nil
	}
	m.ViewContext.Cancel()
}

// Init implements tea.Model.
func (m *ProvidersViewModel) Init() tea.Cmd {
	return tea.Batch(m.fetchProvidersCmd(false), shared.SpinnerTickCmd())
}

// Refresh implements tui.Refresher: reload when a child view pops so install
// and running state reflect anything the child changed. refresh=true because
// the server caches provider state, and the cached copy is exactly what the
// child just invalidated.
func (m *ProvidersViewModel) Refresh() tea.Cmd { return m.fetchProvidersCmd(true) }

func (m *ProvidersViewModel) fetchProvidersCmd(refresh bool) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		apps, err := shared.FetchProviders(client, "", "", refresh)
		return providersLoadedMsg{providers: apps, err: err}
	}
}

func (m *ProvidersViewModel) selectedProvider() *shared.ProviderInfo {
	c := m.list.Cursor()
	if c >= 0 && c < len(m.providers) {
		return &m.providers[c]
	}
	return nil
}

// Update implements tea.Model.
func (m *ProvidersViewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:gocyclo,cyclop // The explicit key, message and rendering cases keep the view flow together.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height - shared.TuiTitleBarLines - shared.TuiBreadcrumbLines
		vis := shared.VisibleRows(m.termHeight, shared.StdListFooter(m.styles))
		m.list.SetVisible(vis)
		m.detail.SetVisible(vis)
		return m, nil

	case tui.BreadcrumbClickMsg:
		if msg.Level == 0 {
			return m, tui.NavBack()
		}
		m.navigateToBreadcrumb(msg.Level)
		return m, nil

	case tea.PasteMsg:
		// Handle paste in key input mode
		if m.mode == providersViewEnterKey {
			pasted := strings.TrimSpace(msg.Content)
			m.keyInput.SetValue(m.keyInput.Value() + pasted)
		}
		if m.mode == providersViewUpgrade {
			pasted := strings.TrimSpace(msg.Content)
			m.upgradeInput.SetValue(m.upgradeInput.Value() + pasted)
		}
		return m, nil

	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		m.handleMouseWheel(msg)
		return m, nil

	case shared.SpinnerTickMsg:
		if m.needsTick() {
			m.loadTick++
			m.refreshUpgradeStatus()
			return m, shared.SpinnerTickCmd()
		}
		return m, nil

	case providersLoadedMsg:
		m.loading = false
		// A failed background reload must not swap populated content for an
		// error page: keep what is on screen and report in the status line.
		if msg.err != nil && len(m.providers) > 0 {
			m.actionStatus = fmt.Sprintf("Refresh failed: %v", msg.err)
			return m, nil
		}
		m.needsRefresh = false
		m.providers = msg.providers
		m.err = msg.err
		// The table now reflects whatever the last action did, so the line
		// describing it is no longer provisional.
		m.actionUnconfirmed = false
		items := make([]tui.Item, len(msg.providers))
		for i, p := range msg.providers {
			items[i] = providerItem{p}
		}
		m.list.SetItems(items)
		return m, nil

	case providersDetailParamsMsg:
		m.detailParamsLoading = false
		m.detailParams = msg.params
		m.detailParamsErr = msg.err
		return m, nil

	case providersStatusMsg:
		m.providerStatus = msg.status
		return m, nil

	case providerVersionsMsg:
		m.applyVersionsReport(msg)
		return m, nil

	case providersCatalogMsg:
		m.loading = false
		if msg.err != nil {
			m.actionStatus = "Server not reachable: start with: zzrouter-node start"
			m.mode = providersViewList
			return m, nil
		}
		m.catalog = msg.catalog
		if len(m.catalog) == 0 {
			typeLabel := "providers"
			if m.installType == constants.AppModeCloud {
				typeLabel = "cloud providers"
			} else if m.installType == "local" {
				typeLabel = "local providers"
			}
			m.actionStatus = fmt.Sprintf("No %s available: check provider configuration", typeLabel)
			m.mode = providersViewList
			return m, nil
		}
		m.buildProviderPicker()
		return m, nil

	case providersActionMsg:
		if msg.err != nil {
			// A failure is already final: no later fetch can change it.
			m.actionStatus = fmt.Sprintf("✗ %s %s failed: %v", msg.action, msg.name, msg.err)
			m.actionUnconfirmed = false
		} else {
			m.actionStatus = fmt.Sprintf("✓ %s %s succeeded", msg.action, msg.name)
			m.actionUnconfirmed = true
		}
		// Refresh the list, bypassing the cache: install/uninstall/upgrade
		// all change the version or presence this table renders.
		m.loading = true
		client := m.client
		return m, tea.Batch(func() tea.Msg {
			apps, err := shared.FetchProviders(client, "", "", true)
			return providersLoadedMsg{providers: apps, err: err}
		}, shared.SpinnerTickCmd())

	case providersPreflightMsg:
		m.loading = false
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("✗ Preflight failed: %v", msg.err)
			m.mode = providersViewList
		} else if !msg.report.AllOK {
			m.preflightReport = msg.report
			m.mode = providersViewPreflightFail
		} else {
			// Preflight passed — proceed to install plan
			m.installPlan = nil
			m.mode = providersViewInstallSteps
			return m, tea.Batch(
				fetchInstallPlan(m.client, m.installProvider, m.installNode),
				shared.SpinnerTickCmd(),
			)
		}
		return m, nil

	case providersInstallPlanMsg:
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("✗ Failed to get install plan: %v", msg.err)
			m.mode = providersViewList
		} else {
			m.installPlan = msg.plan
			m.installStepCursor = 0
			m.installStepStatus = make([]string, len(msg.plan.Steps))
			firstPending := -1
			for i := range m.installStepStatus {
				// Pre-populate from server's current-state probe
				if i < len(msg.plan.CurrentState) && msg.plan.CurrentState[i].Installed {
					m.installStepStatus[i] = stepDone
				} else {
					m.installStepStatus[i] = stepPending
					if firstPending < 0 {
						firstPending = i
					}
				}
			}
			// Position cursor on first pending step
			if firstPending >= 0 {
				m.installStepCursor = firstPending
			}
			m.mode = providersViewInstallSteps
		}
		return m, nil

	case providersStepExecuteMsg:
		// Per-step terminal arrived (via jobstream FrameMsg.Done). Clear
		// the live snapshot + the subscription so the next Enter starts
		// from a clean slate.
		m.installInFlight = false
		m.installProgress = nil
		if m.installJobRow != nil {
			m.installJobRow.Cancel()
			m.installJobRow = nil
		}
		m.installJobStep = 0
		if msg.err != nil {
			if msg.step < len(m.installStepStatus) {
				m.installStepStatus[msg.step] = stepFailed
			}
			m.actionStatus = fmt.Sprintf("✗ Step %d failed: %v", msg.step+1, msg.err)
		} else if msg.result != nil {
			if msg.step < len(m.installStepStatus) {
				if msg.result.Passed {
					m.installStepStatus[msg.step] = stepDone
					// Check if all steps are now done
					allDone := true
					for _, s := range m.installStepStatus {
						if s != stepDone && s != stepVerified {
							allDone = false
							break
						}
					}
					if allDone {
						m.actionStatus = fmt.Sprintf("✓ %s installed successfully", m.installProvider)
						m.needsRefresh = true
						return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg {
							return providersVerifySuccessTimerMsg{}
						})
					}
					m.actionStatus = fmt.Sprintf("✓ Step %d completed", msg.step+1)
					// Auto-advance cursor to next pending step
					for i := msg.step + 1; i < len(m.installStepStatus); i++ {
						if m.installStepStatus[i] == stepPending {
							m.installStepCursor = i
							break
						}
					}
				} else {
					m.installStepStatus[msg.step] = stepFailed
					m.actionStatus = fmt.Sprintf("✗ Step %d: %s", msg.step+1, msg.result.Message)
				}
			}
		}
		return m, nil

	case providersStepVerifyMsg:
		if msg.err != nil {
			if msg.step < len(m.installStepStatus) {
				m.installStepStatus[msg.step] = stepFailed
			}
			m.actionStatus = fmt.Sprintf("✗ Verify step %d failed: %v", msg.step+1, msg.err)
		} else if msg.result != nil {
			if msg.step < len(m.installStepStatus) {
				if msg.result.Passed {
					m.installStepStatus[msg.step] = stepVerified
				} else {
					m.installStepStatus[msg.step] = stepFailed
					m.actionStatus = fmt.Sprintf("✗ Step %d: %s", msg.step+1, msg.result.Message)
				}
			}
		}
		return m, nil

	case providersVerifySuccessTimerMsg:
		// Already back on the list with fresh data (user pressed Esc before timer) — skip
		if m.mode == providersViewList && !m.needsRefresh {
			return m, nil
		}
		// Timer expired after showing success — go back to list and refresh
		m.mode = providersViewList
		m.loading = true
		m.actionStatus = ""
		client := m.client
		return m, tea.Batch(func() tea.Msg {
			apps, err := shared.FetchProviders(client, "", "", true)
			return providersLoadedMsg{providers: apps, err: err}
		}, shared.SpinnerTickCmd())

	case providersVerifyMsg:
		m.loading = false
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("✗ Verification failed: %v", msg.err)
			m.mode = providersViewEnterKey
			m.keyInput.SetValue("")
		} else if msg.result != nil && msg.result.Status == "ok" {
			if m.installType == constants.AppModeCloud {
				// Cloud provider — show success on key screen, wait 3s, then go to list
				m.actionStatus = fmt.Sprintf("✓ %s is verified and ready to use", m.installProvider)
				m.mode = providersViewEnterKey
				m.keyInput.SetValue("")
				return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg {
					return providersVerifySuccessTimerMsg{}
				})
			}
			// Local provider — run preflight checks first
			m.actionStatus = ""
			m.installPlan = nil
			m.loading = true
			return m, tea.Batch(
				fetchPreflight(m.client, m.installProvider, m.installNode),
				shared.SpinnerTickCmd(),
			)
		} else {
			message := "unknown error"
			if msg.result != nil {
				message = msg.result.Message
			}
			m.actionStatus = fmt.Sprintf("✗ Verification failed: %s", message)
			m.mode = providersViewEnterKey // Let user retry with a different key
			m.keyInput.SetValue("")
		}
		return m, nil

	case providersUpgradeJobSubscribedMsg:
		if msg.err != nil {
			return m, func() tea.Msg {
				return providersActionMsg{action: "upgrade", name: msg.name, err: msg.err}
			}
		}
		// Tear down any row still in flight. The prompt returns to the list
		// on enter, so a second upgrade is reachable before the first
		// finishes; without this the old row's frames fail the JobID guard,
		// its read is never re-armed, and its outcome — failure included —
		// is never reported.
		if m.upgrade != nil {
			m.upgrade.row.Cancel()
		}
		m.upgrade = newUpgradeJob(msg.row, msg.name)
		m.actionStatus = fmt.Sprintf("Upgrading %s…", msg.name)
		// Re-arm the tick here, not at launch: the tick batched with the
		// POST fires before this subscription exists, sees no job in
		// flight, and ends the chain.
		return m, tea.Batch(msg.next, shared.SpinnerTickCmd())

	case providersInstallJobSubscribedMsg:
		// Either Run All (stepIdx=-1) or per-step (stepIdx>=0) kicked
		// off. POST error maps to the right terminal handler based on
		// stepIdx; success parks the row + pumps the first frame.
		if msg.err != nil {
			if msg.stepIdx < 0 {
				return m, func() tea.Msg { return providersAutoInstallDoneMsg{err: msg.err} }
			}
			idx := msg.stepIdx
			return m, func() tea.Msg {
				return providersStepExecuteMsg{step: idx, err: msg.err}
			}
		}
		m.installJobRow = msg.row
		m.installJobStep = msg.stepIdx
		return m, msg.next

	case jobstream.FrameMsg:
		// An upgrade job owns its frames before the install wizard sees
		// them; the two never run at once but the guard below would
		// otherwise drop every upgrade frame.
		if cmd, owned := m.handleUpgradeFrame(msg); owned {
			return m, cmd
		}
		// Route by JobID. Messages for rows we don't own (e.g. a late
		// frame after a row was replaced) are dropped.
		if m.installJobRow == nil || msg.JobID != m.installJobRow.JobID {
			return m, nil
		}
		if msg.Done {
			m.installJobRow.Done = true
			// Branch: Run All (stepIdx=-1) fires the wizard-level
			// terminal; per-step fires the single-step handler. Both
			// handlers already exist and will Cancel the row on entry.
			if m.installJobStep < 0 {
				return m, func() tea.Msg { return providersAutoInstallDoneMsg{err: msg.Err} }
			}
			idx := m.installJobStep
			result := stepResultFromFrame(msg)
			return m, func() tea.Msg {
				return providersStepExecuteMsg{step: idx, result: result, err: msg.Err}
			}
		}
		// Progress frame — mirror the legacy InstallProgress shape so
		// the existing view renderer keeps working unchanged.
		step := intFromMeta(msg.Event.Meta, "step")
		total := intFromMeta(msg.Event.Meta, "total_steps")
		if total == 0 && m.installPlan != nil {
			total = len(m.installPlan.Steps)
		}
		var done, bytesTotal int64
		if msg.Event.Bytes != nil {
			done = msg.Event.Bytes.Done
			bytesTotal = msg.Event.Bytes.Total
		}
		m.installProgress = &pkgClient.InstallProgress{
			Provider:   m.installProvider,
			Step:       step,
			TotalSteps: total,
			StepDesc:   msg.Event.Step,
			Percent:    msg.Event.Percent,
			BytesDone:  done,
			BytesTotal: bytesTotal,
		}
		m.reflectInstallStepProgress(step)
		return m, jobstream.Next(m.installJobRow)

	case providersAutoInstallDoneMsg:
		m.installInFlight = false
		if m.installJobRow != nil {
			m.installJobRow.Cancel()
			m.installJobRow = nil
		}
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("✗ Install failed: %v", msg.err)
			for i := range m.installStepStatus {
				if m.installStepStatus[i] == stepRunning {
					m.installStepStatus[i] = stepFailed
				}
			}
			return m, nil
		}
		// Success — mark all steps done, show success message, then auto-transition
		// back to the providers list with a data refresh (same pattern as cloud verify).
		m.actionStatus = fmt.Sprintf("✓ %s installed successfully", m.installProvider)
		m.needsRefresh = true
		for i := range m.installStepStatus {
			m.installStepStatus[i] = stepDone
		}
		m.installProgress = nil
		return m, tea.Tick(3*time.Second, func(time.Time) tea.Msg {
			return providersVerifySuccessTimerMsg{}
		})

	case providersConnectResultMsg:
		return m, m.handleConnectResult(msg)

	case tea.KeyPressMsg:
		switch m.mode {
		case providersViewDetail:
			return m, m.updateDetail(msg)
		case providersViewPickType, providersViewPickProvider, providersViewPickNode:
			return m, m.updatePicker(msg)
		case providersViewPreflightFail:
			return m, m.updatePreflightFail(msg)
		case providersViewInstallSteps:
			return m, m.updateInstallWizard(msg)
		case providersViewEnterKey:
			return m, m.updateEnterKey(msg)
		case providersViewConnectForm:
			return m, m.updateConnectForm(msg)
		case providersViewUpgrade:
			return m, m.updateUpgrade(msg)
		default:
			return m, m.updateList(msg)
		}

	default:
		// Forward non-key messages to textinput for cursor blink
		if m.mode == providersViewEnterKey {
			return m, m.updateEnterKey(msg)
		}
		if m.mode == providersViewConnectForm {
			return m, m.updateConnectForm(msg)
		}
		if m.mode == providersViewUpgrade {
			return m, m.updateUpgrade(msg)
		}
	}

	return m, nil
}

func (m *ProvidersViewModel) updateList(msg tea.KeyPressMsg) tea.Cmd {
	// If a sub-view (install/breadcrumb) flagged a refresh, trigger it now
	if m.needsRefresh && !m.loading {
		m.needsRefresh = false
		m.loading = true
		client := m.client
		return tea.Batch(func() tea.Msg {
			apps, err := shared.FetchProviders(client, "", "", true)
			return providersLoadedMsg{providers: apps, err: err}
		}, shared.SpinnerTickCmd())
	}

	if m.list.UpdateKey(msg) {
		return nil
	}

	switch {
	case key.Matches(msg, tui.ProvidersKeys.Enter):
		if app := m.selectedProvider(); app != nil {
			m.mode = providersViewDetail
			m.detailParams = nil
			m.detailParamsErr = nil
			m.detailParamsLoading = true
			m.detail.Reset()
			m.providerStatus = nil
			m.versionsReport = nil
			client := m.client
			appName := app.Name
			return tea.Batch(
				m.fetchProviderVersions(appName, false),
				func() tea.Msg {
					resp, err := client.GetProviderParameters(appName, true)
					if err != nil {
						return providersDetailParamsMsg{err: err}
					}
					return providersDetailParamsMsg{params: resp.Parameters}
				},
				func() tea.Msg {
					statuses, err := client.ListProviderStatus()
					if err != nil {
						return providersStatusMsg{}
					}
					for i := range statuses {
						if statuses[i].Name == appName {
							return providersStatusMsg{status: &statuses[i]}
						}
					}
					return providersStatusMsg{}
				},
			)
		}
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Install):
		m.actionStatus = ""
		m.buildTypePicker()
		return nil

	case key.Matches(msg, tui.ProvidersKeys.Uninstall):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		m.actionStatus = fmt.Sprintf("Uninstalling %s from %s...", app.Name, app.Node)
		client := m.client
		name, node := app.Name, app.Node
		return func() tea.Msg {
			err := client.UninstallProvider(name, node)
			return providersActionMsg{action: "uninstall", name: name, err: err}
		}

	case key.Matches(msg, tui.ProvidersKeys.Upgrade):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		m.actionStatus = ""
		m.upgradeProvider, m.upgradeNode, m.upgradeCurrent = app.Name, app.Node, app.Version
		m.upgradeLatest, m.upgradeStatus = app.LatestVersion, app.VersionStatus
		m.upgradePin = false
		m.upgradeInput = newVersionInput()
		m.prefillUpgradeInput(app)
		m.mode = providersViewUpgrade
		return m.upgradeInput.Focus()

	case key.Matches(msg, tui.ProvidersKeys.Logs):
		app := m.selectedProvider()
		if app == nil {
			break
		}
		providerName := app.Name
		return func() tea.Msg {
			return tui.LaunchRunLogsMsg{Filter: logsclient.RunFilter{Provider: providerName}}
		}

	case key.Matches(msg, tui.ProvidersKeys.Refresh):
		m.loading = true
		m.actionStatus = ""
		client := m.client
		return func() tea.Msg {
			apps, err := shared.FetchProviders(client, "", "", true)
			return providersLoadedMsg{providers: apps, err: err}
		}
	case key.Matches(msg, tui.ProvidersKeys.Back):
		return tui.NavBack()
	case key.Matches(msg, tui.ProvidersKeys.Quit):
		return tea.Quit
	}

	return nil
}

func (m *ProvidersViewModel) updateDetail(msg tea.KeyPressMsg) tea.Cmd {
	if m.detail.UpdateKey(msg) {
		return nil
	}
	switch {
	case key.Matches(msg, tui.ProvidersKeys.Edit):
		if app := m.selectedProvider(); app != nil {
			appName := app.Name
			return func() tea.Msg {
				return tui.OpenParamsEditorMsg{Provider: appName}
			}
		}
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Back, tui.ProvidersKeys.Enter):
		m.mode = providersViewList
		return nil
	case key.Matches(msg, tui.ProvidersKeys.Quit):
		return tea.Quit
	}
	return nil
}

func (m *ProvidersViewModel) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button == tea.MouseRight {
		if m.mode == providersViewDetail {
			m.mode = providersViewList
			m.detail.Reset()
		} else {
			return tui.NavBack()
		}
		return nil
	}
	if m.mode != providersViewList {
		return nil
	}
	enter, ok := m.list.Click(msg, shared.TuiTableClickY)
	if !ok || !enter {
		return nil
	}
	m.mode = providersViewDetail
	m.detailParams = nil
	m.detailParamsErr = nil
	m.detailParamsLoading = true
	m.detail.Reset()
	m.versionsReport = nil
	client := m.client
	appName := m.providers[m.list.Cursor()].Name
	return tea.Batch(
		m.fetchProviderVersions(appName, false),
		func() tea.Msg {
			resp, err := client.GetProviderParameters(appName, true)
			if err != nil {
				return providersDetailParamsMsg{err: err}
			}
			return providersDetailParamsMsg{params: resp.Parameters}
		},
	)
}

func (m *ProvidersViewModel) handleMouseWheel(msg tea.MouseWheelMsg) {
	if m.mode == providersViewList {
		m.list.UpdateWheel(msg)
	}
}

// Breadcrumb implements tui.Breadcrumber.
func (m *ProvidersViewModel) Breadcrumb() []string {
	switch m.mode {
	case providersViewDetail:
		if p := m.selectedProvider(); p != nil {
			return []string{"Providers", p.Name}
		}
		return []string{"Providers", "Details"}
	case providersViewPickType, providersViewPickProvider, providersViewPickNode:
		return []string{"Providers", "Install"}
	case providersViewPreflightFail:
		return []string{"Providers", "Install", "Preflight"}
	case providersViewInstallSteps:
		if m.installPlan != nil {
			return []string{"Providers", "Install", m.installPlan.Provider}
		}
		return []string{"Providers", "Install"}
	case providersViewEnterKey:
		return []string{"Providers", "Install", m.installProvider}
	case providersViewConnectForm:
		return []string{"Providers", "Add remote Ollama"}
	default:
		return []string{"Providers"}
	}
}

// navigateToBreadcrumb navigates to the state matching breadcrumb level.
func (m *ProvidersViewModel) navigateToBreadcrumb(level int) {
	switch level {
	case 1:
		// Level 1 is detail or install — go back to list
		m.mode = providersViewList
	case 2:
		// Level 2 is install sub-step — go back to install picker
		m.mode = providersViewPickProvider
	}
}

// View implements tea.Model.
func (m *ProvidersViewModel) View() tea.View {
	return shared.RenderChildView(m.styles, m.termWidth, m.Breadcrumb(), m.viewContent())
}

func (m *ProvidersViewModel) viewContent() string {
	width, height := m.termWidth, m.termHeight
	switch m.mode {
	case providersViewDetail:
		return m.viewDetail(width, height)
	case providersViewPickType, providersViewPickProvider, providersViewPickNode:
		return m.viewPicker(width, height)
	case providersViewPreflightFail:
		return m.viewPreflightFail(width, height)
	case providersViewInstallSteps:
		return m.viewInstallWizard(width, height)
	case providersViewEnterKey:
		return m.viewEnterKey(width, height)
	case providersViewConnectForm:
		return m.viewConnectForm(width, height)
	case providersViewUpgrade:
		return m.viewUpgrade(width, height)
	default:
		return m.viewList(width, height)
	}
}
