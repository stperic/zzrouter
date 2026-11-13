package views

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/shared"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func componentWizardPlan() *pkgClient.InstallPlanResponse {
	recipe := &pkgClient.InstallRecipeResponse{SelectedComponents: []string{"manager"}}
	recipe.Recipe.Components = map[string]config.InstallComponent{"manager": {Description: "Manager libraries; Manager stays inactive in shared-user mode.", Requirements: "manager_requirements.txt"}}
	return &pkgClient.InstallPlanResponse{Provider: "comfyui", PlanID: "before", ResolvedInstall: recipe, Steps: []pkgClient.InstallStepInfo{{Number: 1, Description: "Prepare"}}}
}

func TestComponentWizardPreviewsExplicitRemovalBeforeExecution(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/zzrouter/v1/providers/comfyui/install/plan", r.URL.Path)
		require.Equal(t, "worker", r.URL.Query().Get("node"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, []any{}, body["components"])
		require.Equal(t, true, body["force"])
		plan := componentWizardPlan()
		plan.PlanID = "after"
		plan.ResolvedInstall.SelectedComponents = []string{}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": plan}))
	}))
	defer srv.Close()
	client := pkgClient.NewClient(config.ClientNodeConfig{Address: srv.URL})
	m := NewProvidersViewModel(client, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.installPlan, m.installProvider, m.installNode = componentWizardPlan(), "comfyui", "worker"
	m.installStepStatus = []string{stepPending}
	m.mode = providersViewInstallSteps
	require.Nil(t, m.updateInstallWizard(tea.KeyPressMsg{Code: 'o', Text: "o"}))
	require.NotNil(t, m.installComponents)
	require.Nil(t, m.updateInstallWizard(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}))
	cmd := m.updateInstallWizard(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	require.Zero(t, calls, "selection performs no synchronous network operation")
	require.False(t, m.installInFlight)
	msg := cmd().(providersInstallPlanMsg)
	require.NoError(t, msg.err)
	require.True(t, msg.plan.Force)
	require.Equal(t, []string{}, msg.plan.ResolvedInstall.SelectedComponents)
	m.Update(msg)
	require.Equal(t, "after", m.installPlan.PlanID)
	require.Equal(t, 1, calls, "selection only previews; Run All remains explicit")
}

func TestComponentWizardCancelAndNarrowLayout(t *testing.T) {
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.installPlan = componentWizardPlan()
	m.openInstallComponents()
	for _, width := range []int{36, 80, 160} {
		for _, line := range strings.Split(m.viewInstallWizard(width, 30), "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), width-2)
		}
	}
	require.Nil(t, m.updateInstallComponents(tea.KeyPressMsg{Code: tea.KeyEscape}))
	require.Nil(t, m.installComponents)
	require.Equal(t, "before", m.installPlan.PlanID)
	require.Equal(t, []string{"manager"}, m.installPlan.ResolvedInstall.SelectedComponents)
	m.installInFlight = true
	m.updateInstallWizard(tea.KeyPressMsg{Code: 'o', Text: "o"})
	require.Nil(t, m.installComponents, "a running installation cannot change its component selection")
}

func TestInstalledProviderOpensDependencyPreview(t *testing.T) {
	for _, mode := range []providersViewMode{providersViewList, providersViewDetail} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/zzrouter/v1/providers/comfyui/environment", r.URL.Path)
				require.Equal(t, http.MethodGet, r.Method)
				components := componentWizardPlan().ResolvedInstall.Recipe.Components
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"desired_install": map[string]any{"recipe": map[string]any{"components": components}}, "installed_manifest": map[string]any{"selected_components": []string{"manager"}}}}))
			}))
			defer srv.Close()
			m := NewProvidersViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: srv.URL}), ui.NewStyles(ui.CatppuccinMocha()))
			t.Cleanup(m.Cancel)
			provider := shared.ProviderInfo{Name: "comfyui", Node: "worker", Mode: constants.AppModeOnDemand}
			m.providers = []shared.ProviderInfo{provider}
			m.list.SetItems([]tui.Item{providerItem{provider}})
			m.mode = mode
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
			require.NotNil(t, cmd)
			msg := cmd().(providersInstallComponentsMsg)
			require.NoError(t, msg.err)
			m.Update(msg)
			require.Equal(t, providersViewInstallSteps, m.mode)
			require.NotNil(t, m.installComponents)
			require.True(t, m.installComponentForce)
			require.Nil(t, m.installPlan, "discovery must not invent an executable plan")
			require.Equal(t, []string{"manager: " + msg.data.DesiredInstall.Recipe.Components["manager"].Description}, m.installComponents.Selected())
		})
	}
}

func TestComponentPreviewCancellationDiscardsLateResult(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer srv.Close()
	m := NewProvidersViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: srv.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.installProvider, m.installNode = "comfyui", "worker"
	cmd := m.fetchInstallPlan()
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not start")
	}
	m.updateInstallWizard(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("leaving the wizard did not cancel its request")
	}
	var msg providersInstallPlanMsg
	select {
	case response := <-done:
		msg = response.(providersInstallPlanMsg)
	case <-time.After(2 * time.Second):
		t.Fatal("preview did not finish")
	}
	require.ErrorIs(t, msg.err, context.Canceled)
	m.Update(msg)
	require.Equal(t, providersViewList, m.mode)
	require.Nil(t, m.installPlan)
	m.fetchInstallPlan()
	m.Update(providersInstallPlanMsg{plan: componentWizardPlan(), ctx: msg.ctx})
	require.Nil(t, m.installPlan, "a superseded response must not replace the new preview")
}

func TestComponentPreviewOwnershipSurvivesViewReplacement(t *testing.T) {
	old := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	old.fetchInstallPlan()
	late := providersInstallPlanMsg{plan: componentWizardPlan(), ctx: old.installContext}
	old.Cancel()
	current := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(current.Cancel)
	current.installProvider = "other-provider"
	current.fetchInstallPlan()
	request := current.installContext
	current.Update(late)
	require.Nil(t, current.installPlan)
	require.Same(t, request, current.installContext, "another view's reply must not cancel this request")
}

func TestRetiredLastComponentCanBeExplicitlyRemoved(t *testing.T) {
	previews := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"desired_install": map[string]any{"recipe": map[string]any{"components": map[string]any{}}}, "installed_manifest": map[string]any{"selected_components": []string{"retired"}}}}))
			return
		}
		previews++
		require.Equal(t, "/zzrouter/v1/providers/comfyui/install/plan", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, []any{}, body["components"])
		require.Equal(t, true, body["force"])
		plan := componentWizardPlan()
		plan.PlanID = "replacement"
		plan.ResolvedInstall.SelectedComponents = []string{}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": plan}))
	}))
	defer srv.Close()
	m := NewProvidersViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: srv.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	provider := shared.ProviderInfo{Name: "comfyui", Node: "worker", Mode: constants.AppModeOnDemand}
	m.providers = []shared.ProviderInfo{provider}
	m.list.SetItems([]tui.Item{providerItem{provider}})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	m.Update(cmd())
	require.NotNil(t, m.installComponents)
	require.Len(t, m.installComponents.Selected(), 1)
	require.Contains(t, m.installComponents.Selected()[0], "Unavailable in this release")
	m.updateInstallComponents(tea.KeyPressMsg{Code: 'n', Text: "n"})
	cmd = m.updateInstallComponents(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(cmd())
	require.Equal(t, "replacement", m.installPlan.PlanID)
	require.Equal(t, []string{}, m.installPlan.ResolvedInstall.SelectedComponents)
	require.Equal(t, 1, previews)
}

func TestComponentChecklistKeepsFocusedOptionVisibleAfterResize(t *testing.T) {
	m := NewProvidersViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.installPlan = componentWizardPlan()
	for index := range 16 {
		m.installPlan.ResolvedInstall.Recipe.Components[fmt.Sprintf("optional-%02d", index)] = config.InstallComponent{Description: "Optional libraries"}
	}
	m.termHeight = 20
	m.openInstallComponents()
	m.updateInstallComponents(tea.KeyPressMsg{Code: tea.KeyEnd})
	view := m.viewInstallWizard(100, 20)
	require.Contains(t, view, "optional-15")
	require.Contains(t, view, "Enter: preview")
	require.LessOrEqual(t, len(strings.Split(view, "\n")), 20-4)
	before := m.installComponents.Selected()
	m.termHeight = 10
	m.resizeInstallComponents()
	require.Contains(t, m.viewInstallWizard(100, 10), "Resize the terminal")
	m.updateInstallComponents(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	require.Equal(t, before, m.installComponents.Selected(), "hidden options must not change")
}
