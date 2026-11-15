package views

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/access/teams"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/require"
)

func TestAccessDetailConfirmationRemainsVisibleOutsideScroll(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {40, 16}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			styles := ui.NewStyles(ui.CatppuccinMocha())
			team := &pkgClient.TeamResponse{ID: "engineering", Name: "Engineering", Kind: "shared"}
			m := NewTeamsViewModel(nil, styles)
			t.Cleanup(m.Cancel)
			m.teams = []*pkgClient.TeamResponse{team}
			m.list.SetItems([]tui.Item{teamItem{team}})
			m.loading, m.mode = false, teamsViewDetail
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.Update(tea.KeyPressMsg{Code: 's'})
			require.True(t, m.confirmPending)
			require.Contains(t, m.View().Content, "Suspend team")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
			require.Contains(t, m.View().Content, "Suspend team")
			m.Update(tea.KeyPressMsg{Code: 'n'})
			m.statusMsg = "Action result retained"
			require.Contains(t, m.View().Content, "Action result retained")

			key := &pkgClient.KeyResponse{ID: "test-key", Name: "Test", TeamID: "engineering", Role: "user"}
			k := NewVkeysViewModel(nil, styles)
			t.Cleanup(k.Cancel)
			k.keys = []*pkgClient.KeyResponse{key}
			k.list.SetItems([]tui.Item{vkeyItem{key}})
			k.loading, k.mode = false, vkeysViewDetail
			k.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			k.Update(tea.KeyPressMsg{Code: 's'})
			require.True(t, k.confirmPending)
			require.Contains(t, k.View().Content, "Suspend key")
			k.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
			require.Contains(t, k.View().Content, "Suspend key")
			k.Update(tea.KeyPressMsg{Code: 'n'})
			k.statusMsg = "Action result retained"
			require.Contains(t, k.View().Content, "Action result retained")
		})
	}
}

func newKeyPolicyFixture(t *testing.T, handler http.HandlerFunc) *VkeysViewModel {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m := NewVkeysViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.loading = false
	m.openNewKeyForm()
	m.formID.SetValue("test-key")
	m.formName.SetValue("Test key")
	m.teamsLoaded = true
	m.teams = []*pkgClient.TeamResponse{{ID: "engineering", Name: "Engineering", Kind: "shared", ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"ollama@studio", "groq"}}, RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}, AllowedModels: []string{"qwen*"}, SpendLimit: 12.50, ResetPeriod: "monthly"}}
	return m
}

func TestKeyCreationRequiresExplicitActiveSharedTeam(t *testing.T) {
	for _, scenario := range []string{"not selected", "no teams", "personal", "suspended", "loading", "failed"} {
		t.Run(scenario, func(t *testing.T) {
			m := newKeyPolicyFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("no key request should be made") })
			m.formTeamID = "engineering"
			switch scenario {
			case "not selected":
				m.formTeamID = ""
			case "no teams":
				m.teams = nil
			case "personal":
				m.teams[0].Kind = "personal"
			case "suspended":
				m.teams[0].Suspended = true
			case "loading":
				m.teamsLoaded = false
			case "failed":
				m.teamsErr = errors.New("unavailable")
			}
			require.Nil(t, m.saveKeyForm())
			require.Contains(t, m.statusMsg, "active shared team")
			require.False(t, m.pending)
		})
	}
}

func TestKeyCreationUsesTeamAndPreservesOneTimeSecret(t *testing.T) {
	var body map[string]any
	m := newKeyPolicyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"test-key","key":"synthetic-one-time-key"}}`))
	})
	m.formFocus = vkeysFieldTeam
	m.updateKeyForm(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, "engineering", m.formTeamID)
	cmd := m.saveKeyForm()
	require.NotNil(t, cmd)
	require.True(t, m.NavigationBlocked())
	require.Nil(t, m.saveKeyForm(), "a second submission must not start")
	for _, message := range []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEscape}, tui.BreadcrumbClickMsg{Level: 0}, tea.MouseClickMsg{Button: tea.MouseRight}} {
		_, blocked := m.Update(message)
		require.Nil(t, blocked)
		require.Equal(t, vkeysViewNewKey, m.mode)
	}
	m.Update(cmd())
	require.Equal(t, "engineering", body["team_id"])
	require.Equal(t, "member", body["team_role"])
	require.NotContains(t, body, "spend_limit")
	require.Equal(t, vkeysViewShowRaw, m.mode)
	require.False(t, m.pending)
	require.Equal(t, "synthetic-one-time-key", m.rawKeyValue)
	m.Cancel()
	require.Empty(t, m.rawKeyValue)
}

func TestKeyEditOnlyChangesCredentialsAndPreservesExactExpiry(t *testing.T) {
	var body map[string]any
	m := newKeyPolicyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPatch, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"test-key","name":"Renamed","team_id":"personal-existing","spend_limit":31.25,"default_max_tokens":2048}}`))
	})
	expiry := "2030-07-10T14:22:13-04:00"
	existing := &pkgClient.KeyResponse{ID: "test-key", Name: "Old", Role: "user", TeamID: "personal-existing", ExpiresAt: &expiry, SpendLimit: 31.25, DefaultMaxTokens: 2048}
	m.keys = []*pkgClient.KeyResponse{existing}
	m.list.SetItems([]tui.Item{vkeyItem{existing}})
	m.openEditKeyForm(existing)
	m.formName.SetValue("Renamed")
	cmd := m.saveKeyForm()
	require.NotNil(t, cmd)
	m.Update(cmd())
	require.Equal(t, map[string]any{"name": "Renamed", "role": "user", "expires_at": expiry}, body)
	require.Equal(t, 31.25, m.keys[0].SpendLimit)
	require.Equal(t, 2048, m.keys[0].DefaultMaxTokens)
	require.Equal(t, vkeysViewDetail, m.mode)
}

func TestKeyFormRefusesMalformedExpiryAndUncertainResubmission(t *testing.T) {
	m := newKeyPolicyFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected request") })
	m.formTeamID = "engineering"
	for _, expiry := range []string{"2028-02-30", "tomorrow", "2029-12-01T25:00:00Z"} {
		m.formExpires.SetValue(expiry)
		require.Nil(t, m.saveKeyForm())
		require.Contains(t, m.statusMsg, "expiry")
	}
	m.formExpires.SetValue("")
	m.Update(vkeysCreatedMsg{owner: m, err: errors.New("connection lost")})
	require.True(t, m.writeUncertain)
	require.Nil(t, m.saveKeyForm())
	require.Contains(t, m.statusMsg, "unknown")
	expiry := "2030-01-01T00:00:00Z"
	existing := &pkgClient.KeyResponse{ID: "test-key", Name: "Test key", ExpiresAt: &expiry}
	m.keys = []*pkgClient.KeyResponse{existing}
	m.list.SetItems([]tui.Item{vkeyItem{existing}})
	m.openEditKeyForm(existing)
	m.formExpires.SetValue("")
	require.Nil(t, m.saveKeyForm())
	require.Contains(t, m.statusMsg, "cannot remove an expiry")
}

func TestTeamPolicySummariesAndConsoleHandoff(t *testing.T) {
	styles := ui.NewStyles(ui.CatppuccinMocha())
	m := NewTeamsViewModel(nil, styles)
	t.Cleanup(m.Cancel)
	team := &pkgClient.TeamResponse{ID: "team/with space", Name: "Engineering", Kind: "shared", ProviderAccess: teams.ProviderAccess{Mode: "selected", Providers: []string{"removed-provider", "ollama@studio"}}, RouteAccess: teams.RouteAccess{Mode: "selected", Routes: []string{"coding"}}, AllowedModels: []string{"qwen*"}, SpendLimit: 7.50, ResetPeriod: "weekly", RPMLimit: 20}
	m.teams = []*pkgClient.TeamResponse{team}
	m.list.SetItems([]tui.Item{teamItem{team}})
	m.loading = false
	_, cmd := m.Update(keyPress("n"))
	create := cmd().(tui.NavToMsg).View.(*ConsoleViewModel)
	require.Equal(t, pkgClient.ConsoleTarget{Page: "access", Item: "teams", Mode: "create"}, create.target)
	m.mode = teamsViewDetail
	_, cmd = m.Update(keyPress("e"))
	edit := cmd().(tui.NavToMsg).View.(*ConsoleViewModel)
	require.Equal(t, pkgClient.ConsoleTarget{Page: "access", Item: "teams/team/with space"}, edit.target)
	lines := strings.Join(teamPolicyLines(team), "\n")
	for _, expected := range []string{"removed-provider", "ollama@studio", "coding", "qwen*", "$7.50 / weekly", "RPM 20"} {
		require.Contains(t, lines, expected)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	frame := m.View().Content
	require.LessOrEqual(t, lipgloss.Height(frame), 24)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.NotEqual(t, frame, m.View().Content, "policy details must scroll")
}

func TestTeamLifecycleBlocksNavigationAndDoubleSubmission(t *testing.T) {
	m := NewTeamsViewModel(nil, ui.NewStyles(ui.CatppuccinMocha()))
	t.Cleanup(m.Cancel)
	m.teams = []*pkgClient.TeamResponse{{ID: "team", Kind: "shared"}}
	m.list.SetItems([]tui.Item{teamItem{m.teams[0]}})
	m.mode, m.confirmPending, m.confirmAction = teamsViewDetail, true, "suspend"
	_, cmd := m.Update(keyPress("y"))
	require.NotNil(t, cmd)
	require.True(t, m.NavigationBlocked())
	_, duplicate := m.Update(keyPress("y"))
	require.Nil(t, duplicate)
	_, back := m.Update(tui.BreadcrumbClickMsg{Level: 0})
	require.Nil(t, back)
	require.Equal(t, teamsViewDetail, m.mode)
	m.Update(teamsSuspendedMsg{team: &pkgClient.TeamResponse{ID: "team", Suspended: true}})
	require.False(t, m.NavigationBlocked())
}

func TestKeyFormPolicyPreviewScrollsInShortTerminal(t *testing.T) {
	m := newKeyPolicyFixture(t, func(http.ResponseWriter, *http.Request) {})
	m.formTeamID = "engineering"
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	first := m.View().Content
	require.LessOrEqual(t, lipgloss.Width(first), 80)
	require.LessOrEqual(t, lipgloss.Height(first), 24)
	require.Contains(t, first, "Ctrl+S save")
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	last := m.View().Content
	require.Contains(t, last, "$12.50 / monthly")
	require.NotEqual(t, first, last)
}
