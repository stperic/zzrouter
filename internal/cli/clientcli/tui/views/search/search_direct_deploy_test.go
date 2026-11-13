package search

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func beginDirectValidation(t *testing.T, m *SearchTUIModel) tea.Cmd {
	t.Helper()
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	require.Equal(t, stateDirectPull, m.State)
	m.directPullInput = "org/model"
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	require.True(t, m.directPullLoading)
	return cmd
}

func TestDirectDeployPendingValidationCanCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
		quit bool
	}{
		{name: "escape", msg: tea.KeyPressMsg{Code: tea.KeyEscape}},
		{name: "right-click", msg: tea.MouseClickMsg{Button: tea.MouseRight}},
		{name: "quit", msg: tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, quit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			reply := make(chan tea.Msg, 1)
			f := &discoveryFixture{card: func(ctx context.Context, _, _ string) (*metadata.DiscoveryResult, error) {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			m := activeSearch(t, f)
			cmd := beginDirectValidation(t, m)
			go func() { reply <- cmd() }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("validation request did not start")
			}
			_, _ = m.Update(tc.msg)
			require.False(t, m.directPullLoading, "cancellation retires validation's loading state")
			select {
			case msg := <-reply:
				require.ErrorIs(t, msg.(deployValidateMsg).err, context.Canceled)
				_, _ = m.Update(msg)
			case <-time.After(time.Second):
				t.Fatal("cancellation did not reach the validation request")
			}
			assert.Empty(t, m.directPullError, "a canceled reply must not become a form error")
			if tc.quit {
				assert.False(t, m.active)
				return
			}
			require.Equal(t, StateList, m.State)
			assert.Empty(t, m.directPullInput)
			_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
			_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			assert.Equal(t, StateList, m.State, "reopened overlay still accepts back")
		})
	}
}

func TestDirectDeployRetiredReplyCannotClearReplacementLoading(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		if failed {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			f := &discoveryFixture{card: func(context.Context, string, string) (*metadata.DiscoveryResult, error) {
				if failed {
					return nil, errors.New("queued validation error")
				}
				return &metadata.DiscoveryResult{}, nil
			}}
			m := activeSearch(t, f)
			queued := beginDirectValidation(t, m)()
			_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Equal(t, StateList, m.State)
			current := beginDirectValidation(t, m)
			_, _ = m.Update(queued)
			require.Equal(t, stateDirectPull, m.State)
			require.True(t, m.directPullLoading, "old completion cannot retire replacement work")
			_, duplicate := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			assert.Nil(t, duplicate, "the replacement remains protected from duplicate submission")
			_, _ = m.Update(tea.KeyPressMsg{Code: 'x'})
			assert.Equal(t, "org/model", m.directPullInput, "typing remains disabled during validation")
			assert.Empty(t, m.directPullError)
			_, _ = m.Update(current())
			assert.False(t, m.directPullLoading, "the current completion clears its own loading state")
		})
	}
}

func TestDirectDeployReentryRetiresValidationLoading(t *testing.T) {
	f := &discoveryFixture{card: func(ctx context.Context, _, _ string) (*metadata.DiscoveryResult, error) {
		return nil, ctx.Err()
	}}
	m := activeSearch(t, f)
	cmd := beginDirectValidation(t, m)
	m.Deactivate()
	m.State = StateList
	_ = m.Activate()
	_, _ = m.Update(cmd())
	_, _ = m.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	require.Equal(t, stateDirectPull, m.State)
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, StateList, m.State, "reentry must not retain a retired request's loading state")
}
