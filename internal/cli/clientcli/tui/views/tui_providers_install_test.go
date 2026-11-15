package views

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func TestHookOnlyExportExplainsServerExecutionAndKeepsGuidedActions(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'a', Text: "a"}, {Code: tea.KeyEnter}} {
		t.Run(key.String(), func(t *testing.T) {
			m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			m.installPlan = &pkgClient.InstallPlanResponse{Provider: "engine", Steps: []pkgClient.InstallStepInfo{{Number: 1, Description: "Prepare", Command: "echo prepare"}, {Number: 2, Description: "Activate"}}}
			m.installPlan.ScriptExport = &pkgClient.InstallScriptExport{Reason: "Step 2 requires managed execution."}
			m.installStepStatus = []string{stepPending, stepPending}
			m.installStepCursor = 1
			require.Nil(t, m.updateInstallWizard(tea.KeyPressMsg{Code: 'x', Text: "x"}))
			require.Equal(t, m.installPlan.ScriptExport.Reason, m.actionStatus)
			require.Contains(t, m.viewInstallWizard(160, 32), m.actionStatus)
			require.False(t, m.installInFlight)
			require.Equal(t, []string{stepPending, stepPending}, m.installStepStatus)
			require.NotNil(t, m.updateInstallWizard(key), "guided server execution remains available")
			require.True(t, m.installInFlight)
			require.Equal(t, stepRunning, m.installStepStatus[1])
		})
	}
}
