package search

import (
	"context"
	"errors"
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stretchr/testify/require"
)

func TestDeploymentPendingWritePreservesResponseAndBlocksDuplicate(t *testing.T) {
	started, release := make(chan context.Context, 1), make(chan struct{})
	calls := 0
	fixture := &discoveryFixture{deploy: func(ctx context.Context, _ pkgClient.DeployRequest) (*pkgClient.Deployment, error) {
		calls++
		started <- ctx
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &pkgClient.Deployment{ID: "deployment-owned", Nodes: []pkgClient.DeploymentNode{{Node: "worker", JobID: "job-owned"}}}, nil
	}}
	m := activeSearch(t, fixture)
	m.State = StateDetail
	m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
	command := m.deployModelToNodes([]string{"worker"})
	done := make(chan tea.Msg, 1)
	go func() { done <- command() }()
	ctx := <-started
	_, bounded := ctx.Deadline()
	require.True(t, bounded)
	for _, message := range []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
		tea.KeyPressMsg{Code: 'm'}, tea.KeyPressMsg{Code: 'p'}, tea.MouseClickMsg{Button: tea.MouseRight}, tui.BreadcrumbClickMsg{Level: 0},
	} {
		_, next := m.Update(message)
		require.Nil(t, next)
		require.True(t, m.NavigationBlocked())
		require.NoError(t, ctx.Err())
		require.Equal(t, statePulling, m.State)
	}
	require.Nil(t, m.quitOrBack())
	require.Nil(t, m.fetchResults())
	require.Nil(t, m.deployModelToNodes([]string{"worker"}))
	close(release)
	_, _ = m.Update(<-done)
	require.False(t, m.NavigationBlocked())
	require.Equal(t, 1, calls)
	require.Equal(t, "deployment-owned", m.LastDeployment.ID)
	require.Equal(t, "job-owned", m.LastDeployment.Nodes[0].JobID)
	require.Contains(t, m.PullStatus, "See Activity")
	require.Equal(t, StateDetail, m.State)
}

func TestDeploymentUnknownOutcomeReleasesNavigationWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		name, want string
		err        error
	}{
		{"deadline", "outcome unknown", context.DeadlineExceeded},
		{"transport", "outcome unknown", errors.New("response lost")},
		{"server", "outcome unknown", &pkgClient.APIError{StatusCode: http.StatusInternalServerError}},
		{"refused", "Deploy refused", &pkgClient.APIError{StatusCode: http.StatusConflict}},
		{"empty-reply", "outcome unknown", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := &discoveryFixture{deploy: func(ctx context.Context, _ pkgClient.DeployRequest) (*pkgClient.Deployment, error) {
				calls++
				_, bounded := ctx.Deadline()
				require.True(t, bounded)
				return nil, test.err
			}}
			m := activeSearch(t, fixture)
			m.selectedItem = &searchResult{Provider: "huggingface", Name: "org/model"}
			_, _ = m.Update(m.deployModelToNodes(nil)())
			require.False(t, m.NavigationBlocked())
			require.Contains(t, m.PullStatus, test.want)
			if test.want == "outcome unknown" {
				require.Contains(t, m.PullStatus, "Check Activity before retrying")
			}
			require.Equal(t, 1, calls)
			require.Nil(t, m.LastDeployment)
		})
	}
}
