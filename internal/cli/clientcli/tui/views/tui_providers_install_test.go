package views

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func TestInstallScriptRequiresEveryStepToHaveACommand(t *testing.T) {
	for _, commands := range [][]string{{"echo prepare", "echo install"}, {""}, {"", "echo install"}, {"echo prepare", ""}} {
		t.Run(strings.Join(commands, ","), func(t *testing.T) {
			plan := &pkgClient.InstallPlanResponse{Provider: "engine"}
			complete := true
			for i, command := range commands {
				plan.Steps = append(plan.Steps, pkgClient.InstallStepInfo{Number: i + 1, Description: "Install", Command: command})
				complete = complete && command != ""
			}
			script, err := buildInstallScript(plan)
			if !complete {
				require.EqualError(t, err, "This plan requires server execution; use Run All or run each step.")
				require.Empty(t, script, "partial scripts must never be exported")
				return
			}
			require.NoError(t, err)
			require.Equal(t, "#!/bin/bash\nset -e\n# zzRouter install script for engine\n\n# Step 1: Install\necho prepare\n\n# Step 2: Install\necho install\n\n", script)
		})
	}
}

func TestHookOnlyExportExplainsServerExecutionAndKeepsGuidedActions(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'a', Text: "a"}, {Code: tea.KeyEnter}} {
		t.Run(key.String(), func(t *testing.T) {
			m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			m.installPlan = &pkgClient.InstallPlanResponse{Provider: "engine", Steps: []pkgClient.InstallStepInfo{{Number: 1, Description: "Prepare", Command: "echo prepare"}, {Number: 2, Description: "Activate"}}}
			m.installStepStatus = []string{stepPending, stepPending}
			m.installStepCursor = 1
			require.Nil(t, m.updateInstallWizard(tea.KeyPressMsg{Code: 'x', Text: "x"}))
			require.Equal(t, "This plan requires server execution; use Run All or run each step.", m.actionStatus)
			require.Contains(t, m.viewInstallWizard(160, 32), m.actionStatus)
			require.False(t, m.installInFlight)
			require.Equal(t, []string{stepPending, stepPending}, m.installStepStatus)
			require.NotNil(t, m.updateInstallWizard(key), "guided server execution remains available")
			require.True(t, m.installInFlight)
			require.Equal(t, stepRunning, m.installStepStatus[1])
		})
	}
}
