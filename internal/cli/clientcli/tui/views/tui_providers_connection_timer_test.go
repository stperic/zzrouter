package views

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	client "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderConnectionWriteIgnoresRetiredInstallTimer(t *testing.T) {
	for _, completion := range []struct {
		name string
		msg  tea.Msg
	}{
		{"single-step", providersStepExecuteMsg{step: 0, result: &client.StepResultResponse{Passed: true}}},
		{"run-all", providersAutoInstallDoneMsg{}},
	} {
		t.Run(completion.name, func(t *testing.T) {
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
			m.mode = providersViewInstallSteps
			m.installPlan = &client.InstallPlanResponse{Provider: "fixture", Steps: []client.InstallStepInfo{{Number: 1}}}
			m.installStepStatus = []string{stepRunning}
			ctx := m.beginInstallRequest()
			m.installInFlight = true
			_, timer := m.Update(installReplyContext(ctx, completion.msg))
			require.NotNil(t, timer, "install completion schedules the delayed transition")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Equal(t, providersViewList, m.mode)
			m.Update(m.beginConnection("fixture")())
			_, write := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			require.NotNil(t, write)
			done := make(chan tea.Msg, 1)
			go func() { done <- write() }()
			<-started
			require.True(t, m.NavigationBlocked())
			_, refresh := m.Update(providersVerifySuccessTimerMsg{ctx: ctx})
			assert.Nil(t, refresh, "the prior wizard's timer cannot launch a refresh from a connection form")
			assert.Equal(t, providersViewEnterKey, m.mode)
			unblock()
			m.Update(<-done)
			assert.False(t, m.NavigationBlocked(), "the write reply must release the navigation guard")
			assert.False(t, m.connectionBusy)
			assert.Equal(t, uint64(8), m.connection.Revision)
			assert.Contains(t, m.actionStatus, "saved")
		})
	}
}

func TestProviderInstallTimerStillCompletesCurrentWizard(t *testing.T) {
	m := connectionWriteFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("Update must schedule I/O") })
	m.mode = providersViewInstallSteps
	m.needsRefresh = true
	ctx := m.beginInstallRequest()
	_, refresh := m.Update(providersVerifySuccessTimerMsg{ctx: ctx})
	require.NotNil(t, refresh)
	require.Equal(t, providersViewList, m.mode)
	require.True(t, m.loading)
}
