package views

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui/jobstream"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderAsyncOwnershipRetiredRepliesDuringWrite(t *testing.T) {
	plan := &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
	for _, tc := range []struct {
		name string
		mode providersViewMode
		msg  tea.Msg
	}{
		{"catalog", providersViewPickType, providersCatalogMsg{catalog: []client.ProviderCatalogEntry{{Key: "retired"}}}},
		{"catalog-empty", providersViewPickType, providersCatalogMsg{}},
		{"catalog-error", providersViewPickType, providersCatalogMsg{err: assert.AnError}},
		{"preflight-pass", providersViewPickNode, providersPreflightMsg{report: &client.PreflightResponse{Provider: "fixture", AllOK: true}}},
		{"preflight-fail", providersViewPickNode, providersPreflightMsg{report: &client.PreflightResponse{Provider: "fixture", AllOK: false}}},
		{"preflight-error", providersViewPickNode, providersPreflightMsg{err: assert.AnError}},
		{"plan", providersViewInstallSteps, providersInstallPlanMsg{plan: plan}},
		{"plan-error", providersViewInstallSteps, providersInstallPlanMsg{err: assert.AnError}},
		{"step-execute-error", providersViewInstallSteps, providersStepExecuteMsg{step: 0, err: assert.AnError}},
		{"step-verify-error", providersViewInstallSteps, providersStepVerifyMsg{step: 0, err: assert.AnError}},
		{"run-all-terminal", providersViewInstallSteps, providersAutoInstallDoneMsg{}},
		{"subscription-row", providersViewInstallSteps, providersInstallJobSubscribedMsg{row: &jobstream.Row{JobID: "retired"}, next: func() tea.Msg { return nil }}},
		{"subscription-error", providersViewInstallSteps, providersInstallJobSubscribedMsg{err: assert.AnError}},
		{"timer-control", providersViewInstallSteps, providersVerifySuccessTimerMsg{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				status := &client.ProviderConnection{Provider: "fixture", Revision: 7, ManagedAvailable: true}
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(map[string]any{"data": status})
					return
				}
				close(started)
				<-release
				status.Revision = 8
				_ = json.NewEncoder(w).Encode(map[string]any{"data": client.ConnectionApplyResult{Connection: status, Saved: true, Durable: true, Outcome: "saved"}})
			})
			t.Cleanup(unblock)
			ctx := m.beginInstallRequest()
			m.mode, m.installLoading = tc.mode, true
			if tc.mode == providersViewInstallSteps && tc.name != "plan" && tc.name != "plan-error" {
				m.installPlan = plan
				m.installStepStatus = []string{stepRunning}
				m.installInFlight = true
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			m.Update(m.beginConnection("fixture")())
			_, write := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			require.NotNil(t, write)
			done := make(chan tea.Msg, 1)
			go func() { done <- write() }()
			<-started
			current, status := m.connectionContext, m.actionStatus
			_, next := m.Update(installReplyContext(ctx, tc.msg))
			assert.Nil(t, next, "retired install replies cannot start another workflow command")
			assert.Equal(t, providersViewEnterKey, m.mode)
			assert.Same(t, current, m.connectionContext)
			assert.Equal(t, status, m.actionStatus, "retired flow cannot replace pending connection feedback")
			assert.Nil(t, m.installJobRow, "retired subscription cannot be adopted")
			unblock()
			m.Update(<-done)
			assert.False(t, m.NavigationBlocked())
			assert.False(t, m.connectionBusy)
			assert.Equal(t, uint64(8), m.connection.Revision)
		})
	}
}

func TestProviderAsyncOwnershipActiveFlowControls(t *testing.T) {
	plan := &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
	for _, tc := range []struct {
		name       string
		mode, want providersViewMode
		msg        tea.Msg
	}{
		{"catalog", providersViewPickType, providersViewPickProvider, providersCatalogMsg{catalog: []client.ProviderCatalogEntry{{Key: "fixture"}}}},
		{"catalog-empty", providersViewPickType, providersViewList, providersCatalogMsg{}},
		{"catalog-error", providersViewPickType, providersViewList, providersCatalogMsg{err: assert.AnError}},
		{"preflight-pass", providersViewPickNode, providersViewInstallSteps, providersPreflightMsg{report: &client.PreflightResponse{AllOK: true}}},
		{"preflight-fail", providersViewPickNode, providersViewPreflightFail, providersPreflightMsg{report: &client.PreflightResponse{AllOK: false}}},
		{"preflight-error", providersViewPickNode, providersViewList, providersPreflightMsg{err: assert.AnError}},
		{"preflight-retry", providersViewPreflightFail, providersViewInstallSteps, providersPreflightMsg{report: &client.PreflightResponse{AllOK: true}}},
		{"plan", providersViewInstallSteps, providersViewInstallSteps, providersInstallPlanMsg{plan: plan}},
		{"plan-error", providersViewInstallSteps, providersViewList, providersInstallPlanMsg{err: assert.AnError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("Update must schedule HTTP") })
			ctx := m.beginInstallRequest()
			m.mode, m.installLoading = tc.mode, true
			if tc.name == "preflight-retry" {
				_, retry := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
				require.NotNil(t, retry)
				require.Equal(t, providersViewPickNode, m.mode)
				ctx = m.installContext
			}
			m.Update(installReplyContext(ctx, tc.msg))
			assert.Equal(t, tc.want, m.mode)
			if tc.name == "plan" {
				assert.Equal(t, plan, m.installPlan)
			}
		})
	}
}

func TestProviderAsyncOwnershipListCannotRetirePickerLoad(t *testing.T) {
	for _, mode := range []providersViewMode{providersViewPickType, providersViewPickNode} {
		m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("no HTTP expected") })
		m.beginInstallRequest()
		m.mode, m.installLoading = mode, true
		m.Update(providersLoadedMsg{providers: []shared.ProviderInfo{{Name: "background"}}})
		assert.True(t, m.installLoading, "background list response must not clear active install load")
	}
}

func TestProviderAsyncOwnershipCatalogReentry(t *testing.T) {
	m := connectionWriteFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []client.ProviderCatalogEntry{{Key: "local-old", Mode: "external"}, {Key: "cloud-new", Mode: "cloud", IsCloud: true}}})
	})
	m.buildTypePicker()
	m.pickerCursor = 1
	_, old := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, old)
	queued := old().(tea.BatchMsg)[0]()
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.buildTypePicker()
	m.pickerCursor = 0
	_, fresh := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, fresh)
	require.Equal(t, providersViewPickType, m.mode)
	require.True(t, m.installLoading)
	m.Update(queued)
	assert.Equal(t, providersViewPickType, m.mode, "a same-mode old Local request cannot satisfy the new Cloud request")
	assert.True(t, m.installLoading)
	assert.Empty(t, m.catalog)
}

func installReplyContext(ctx context.Context, message tea.Msg) tea.Msg {
	switch msg := message.(type) {
	case providersCatalogMsg:
		msg.ctx = ctx
		return msg
	case providersPreflightMsg:
		msg.ctx = ctx
		return msg
	case providersInstallPlanMsg:
		msg.ctx = ctx
		return msg
	case providersStepExecuteMsg:
		msg.ctx = ctx
		return msg
	case providersStepVerifyMsg:
		msg.ctx = ctx
		return msg
	case providersAutoInstallDoneMsg:
		msg.ctx = ctx
		return msg
	case providersInstallJobSubscribedMsg:
		msg.ctx = ctx
		return msg
	case providersVerifySuccessTimerMsg:
		msg.ctx = ctx
		return msg
	default:
		panic("unexpected install reply fixture")
	}
}

func TestProviderAsyncOwnershipSetupReentry(t *testing.T) {
	for _, stage := range []string{"preflight", "plan"} {
		t.Run(stage, func(t *testing.T) {
			m := connectionWriteFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				var data any = &client.PreflightResponse{Provider: "fixture", AllOK: true}
				if stage == "plan" {
					data = &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			})
			request := func() tea.Cmd {
				ctx := m.beginInstallRequest()
				if stage == "preflight" {
					m.mode, m.installLoading = providersViewPickNode, true
					return fetchPreflight(ctx, m.client, "fixture", "node")
				}
				m.mode, m.installPlan = providersViewInstallSteps, nil
				return fetchInstallPlan(ctx, m.client, "fixture", "node")
			}
			queued := request()()
			old := m.installContext
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Error(t, old.Err())
			fresh := request()
			current := m.installContext
			m.Update(queued)
			assert.Same(t, current, m.installContext)
			assert.Nil(t, m.installPlan, "a retired same-mode request cannot satisfy the new load")
			if stage == "preflight" {
				assert.Equal(t, providersViewPickNode, m.mode)
				assert.True(t, m.installLoading)
			}
			m.Update(fresh())
			assert.Equal(t, providersViewInstallSteps, m.mode)
			if stage == "plan" {
				require.NotNil(t, m.installPlan)
			}
		})
	}
}

func TestProviderAsyncOwnershipVerifyPreservesRunningInstall(t *testing.T) {
	m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/zzrouter/v1/providers/fixture/install/verify-step", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": client.StepResultResponse{Step: 2, Passed: true}})
	})
	ctx := m.beginInstallRequest()
	m.mode = providersViewInstallSteps
	m.installPlan = &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}, {Number: 2}}}
	m.installStepStatus = []string{stepRunning, stepPending}
	m.installStepCursor = 1
	m.installInFlight = true
	row := &jobstream.Row{JobID: "active"}
	m.installJobRow, m.installJobStep = row, 0
	_, verify := m.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	require.NotNil(t, verify)
	require.Same(t, ctx, m.installContext)
	require.Same(t, row, m.installJobRow)
	require.NoError(t, ctx.Err())
	m.Update(jobstream.FrameMsg{JobID: "active", Event: client.JobEvent{Step: "installing", Percent: 20, Meta: map[string]any{"step": float64(1)}}})
	require.NotNil(t, m.installProgress)
	assert.Equal(t, 20, m.installProgress.Percent)
	m.Update(verify())
	assert.Equal(t, []string{stepRunning, stepVerified}, m.installStepStatus)
	assert.True(t, m.installInFlight)
	assert.NoError(t, ctx.Err())
	_, terminal := m.Update(jobstream.FrameMsg{JobID: "active", Done: true, Event: client.JobEvent{Meta: map[string]any{"result": map[string]any{"passed": true}}}})
	require.NotNil(t, terminal)
	_, timer := m.Update(terminal())
	require.NotNil(t, timer)
	assert.False(t, m.installInFlight)
	assert.Equal(t, []string{stepDone, stepVerified}, m.installStepStatus)
	assert.Contains(t, m.actionStatus, "installed successfully")
}

func TestProviderAsyncOwnershipCurrentJobSubscription(t *testing.T) {
	for _, singleStep := range []bool{true, false} {
		t.Run(map[bool]string{true: "single-step", false: "run-all"}[singleStep], func(t *testing.T) {
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"job_id": "active", "node": "node"}})
			})
			ctx := m.beginInstallRequest()
			m.mode, m.installInFlight = providersViewInstallSteps, true
			m.installPlan = &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
			m.installStepStatus = []string{stepRunning}
			command := startInstallJobCmd(ctx, m.client, "fixture", "node", *m.installPlan)
			if singleStep {
				command = startExecuteStepJobCmd(ctx, m.client, "fixture", "node", 1, 0, *m.installPlan)
			}
			reply := command().(providersInstallJobSubscribedMsg)
			require.NoError(t, reply.err)
			require.Same(t, ctx, reply.ctx)
			require.NotNil(t, reply.row)
			_, next := m.Update(reply)
			require.NotNil(t, next)
			require.Same(t, reply.row, m.installJobRow)
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			assert.Nil(t, m.installJobRow)
			assert.Error(t, ctx.Err())
		})
	}
}

func TestProviderAsyncOwnershipWizardReentry(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"execute", providersStepExecuteMsg{step: 0, err: assert.AnError}},
		{"verify", providersStepVerifyMsg{step: 0, result: &client.StepResultResponse{Passed: true}}},
		{"run-all", providersAutoInstallDoneMsg{}},
		{"subscription", providersInstallJobSubscribedMsg{row: &jobstream.Row{JobID: "retired"}}},
		{"subscription-error", providersInstallJobSubscribedMsg{err: assert.AnError}},
		{"timer", providersVerifySuccessTimerMsg{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("no HTTP expected") })
			old := m.beginInstallRequest()
			m.mode = providersViewInstallSteps
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			current := m.beginInstallRequest()
			m.mode = providersViewInstallSteps
			m.installPlan = &client.InstallPlanResponse{Provider: "current", Steps: []client.InstallStepInfo{{Number: 1}}}
			m.installStepStatus, m.installInFlight = []string{stepRunning}, true
			row := &jobstream.Row{JobID: "current"}
			m.installJobRow, m.actionStatus = row, "current install"
			_, next := m.Update(installReplyContext(old, tc.msg))
			assert.Nil(t, next)
			assert.Same(t, current, m.installContext)
			assert.Same(t, row, m.installJobRow)
			assert.True(t, m.installInFlight)
			assert.Equal(t, []string{stepRunning}, m.installStepStatus)
			assert.Equal(t, "current install", m.actionStatus)
			assert.Equal(t, providersViewInstallSteps, m.mode)
		})
	}
}

func TestProviderAsyncOwnershipQueuedTerminalCannotClaimReplacement(t *testing.T) {
	m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("no HTTP expected") })
	m.beginInstallRequest()
	m.mode = providersViewInstallSteps
	m.installPlan = &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
	m.installStepStatus, m.installInFlight = []string{stepRunning}, true
	m.installJobRow, m.installJobStep = &jobstream.Row{JobID: "retired"}, 0
	_, queued := m.Update(jobstream.FrameMsg{JobID: "retired", Done: true, Err: assert.AnError})
	require.NotNil(t, queued)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	current := m.beginInstallRequest()
	m.mode = providersViewInstallSteps
	m.installPlan = &client.InstallPlanResponse{Provider: "current", Steps: []client.InstallStepInfo{{Number: 1}}}
	m.installStepStatus, m.installInFlight = []string{stepRunning}, true
	row := &jobstream.Row{JobID: "current"}
	m.installJobRow, m.actionStatus = row, "current install"
	m.Update(queued())
	assert.Same(t, current, m.installContext)
	assert.Same(t, row, m.installJobRow)
	assert.True(t, m.installInFlight)
	assert.Equal(t, "current install", m.actionStatus)
}

func TestProviderAsyncOwnershipRefreshFinishesOutsideList(t *testing.T) {
	for _, target := range []struct {
		name string
		key  tea.KeyPressMsg
		mode providersViewMode
		fail bool
	}{
		{"detail/success", tea.KeyPressMsg{Code: tea.KeyEnter}, providersViewDetail, false},
		{"detail/error", tea.KeyPressMsg{Code: tea.KeyEnter}, providersViewDetail, true},
		{"upgrade/success", tea.KeyPressMsg{Code: 'U', Text: "U"}, providersViewUpgrade, false},
		{"upgrade/error", tea.KeyPressMsg{Code: 'U', Text: "U"}, providersViewUpgrade, true},
	} {
		t.Run(target.name, func(t *testing.T) {
			m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/zzrouter/v1/providers", r.URL.Path)
				assert.Equal(t, "true", r.URL.Query().Get("refresh"))
				if target.fail {
					http.Error(w, "refresh unavailable", http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"name": "fixture", "node": "node", "version": "2"}}})
			})
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			m.mode = providersViewList
			m.Update(providersLoadedMsg{providers: []shared.ProviderInfo{{Name: "fixture", Node: "node", Version: "1"}}})
			_, refresh := m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
			require.NotNil(t, refresh)
			require.True(t, m.loading)
			m.Update(target.key)
			require.Equal(t, target.mode, m.mode)
			m.Update(refresh())
			require.Len(t, m.providers, 1)
			if target.fail {
				assert.Equal(t, "1", m.providers[0].Version)
				assert.Contains(t, m.actionStatus, "Refresh failed")
			} else {
				assert.Equal(t, "2", m.providers[0].Version)
			}
			assert.False(t, m.loading, "the completed list request clears its indicator in every screen")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Equal(t, providersViewList, m.mode)
			assert.False(t, m.loading)
			assert.False(t, m.needsTick(), "no loading owner remains pending")
			assert.Contains(t, m.viewList(100, 24), "fixture", "the refreshed rows remain visible after returning")
		})
	}
}

func TestProviderAsyncOwnershipActionRefreshStartsOutsideList(t *testing.T) {
	for _, mode := range []providersViewMode{providersViewPickType, providersViewPickNode} {
		m := connectionWriteFixture(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/zzrouter/v1/providers", r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"name": "background"}}})
		})
		ctx := m.beginInstallRequest()
		m.mode, m.loading, m.installLoading = mode, false, true
		_, refresh := m.Update(providersActionMsg{action: "upgrade", name: "background"})
		require.NotNil(t, refresh)
		assert.True(t, m.loading, "a background action starts its own list fetch")
		assert.True(t, m.installLoading)
		m.Update(refresh().(tea.BatchMsg)[0]())
		assert.False(t, m.loading)
		assert.True(t, m.installLoading, "list completion cannot retire the pending setup load")
		assert.Same(t, ctx, m.installContext)
		assert.NoError(t, ctx.Err())
		assert.True(t, m.needsTick())
	}
}

func TestProviderAsyncOwnershipLoadingOwnersCompleteIndependently(t *testing.T) {
	for _, mode := range []providersViewMode{providersViewPickType, providersViewPickNode} {
		for _, finish := range []string{"list-first", "setup-first", "back"} {
			t.Run(fmt.Sprintf("%d/%s", mode, finish), func(t *testing.T) {
				m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("Update only schedules HTTP") })
				ctx := m.beginInstallRequest()
				m.mode = mode
				m.loading, m.installLoading = true, true
				var reply tea.Msg = providersCatalogMsg{ctx: ctx, catalog: []client.ProviderCatalogEntry{{Key: "fixture"}}}
				if mode == providersViewPickNode {
					reply = providersPreflightMsg{ctx: ctx, report: &client.PreflightResponse{Provider: "fixture", AllOK: false}}
				}
				list := providersLoadedMsg{providers: []shared.ProviderInfo{{Name: "background"}}}
				if finish == "list-first" {
					m.Update(list)
					assert.False(t, m.loading)
					assert.True(t, m.installLoading)
					assert.True(t, m.needsTick())
					m.Update(reply)
				} else {
					if finish == "back" {
						m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
						assert.Error(t, ctx.Err())
					} else {
						m.Update(reply)
					}
					assert.True(t, m.loading, "setup completion or Back cannot finish a list fetch")
					assert.False(t, m.installLoading)
					assert.True(t, m.needsTick())
					m.Update(list)
				}
				assert.False(t, m.loading)
				assert.False(t, m.installLoading)
				assert.False(t, m.needsTick(), "both owners finished")
			})
		}
	}
}
