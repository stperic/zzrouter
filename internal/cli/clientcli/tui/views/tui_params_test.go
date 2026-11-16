package views

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stperic/zzrouter/internal/cli/clientcli/tui"

	tea "charm.land/bubbletea/v2"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type paramsFixture struct {
	sync.Mutex
	temperature               string
	readFailure, writeFailure bool
	patch                     map[string]any
}

func newParamsFixture(t *testing.T) (*ParamsViewModel, *paramsFixture) {
	t.Helper()
	f := &paramsFixture{temperature: "0.5"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Lock()
		defer f.Unlock()
		switch {
		case r.Method == http.MethodPatch:
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&f.patch))
			if f.writeFailure {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			f.temperature = "0.7"
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/zzrouter/v1/providers/test/schema":
			_, _ = io.WriteString(w, `{"parameters":{"temperature":{"type":"float"},"ctx-size":{"type":"int"}},"environment":{}}`)
		case r.URL.Path == "/zzrouter/v1/providers/test/resolved":
			_, _ = io.WriteString(w, `{"parameters":{"ctx-size":{"value":"4096","tier":"default"}},"environment":{}}`)
		default:
			assert.Equal(t, "model", r.URL.Query().Get("scope"))
			if f.readFailure {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"scope": "model", "parameters": map[string]string{"temperature": f.temperature}, "environment": map[string]string{}}))
		}
	}))
	t.Cleanup(server.Close)
	m := NewParamsViewModel(pkgClient.NewClient(config.ClientNodeConfig{Address: server.URL}), ui.NewStyles(ui.ClaudeCodeTheme()), "test", "model", "")
	m.node = "worker"
	t.Cleanup(m.Cancel)
	m.Update(m.Init()())
	require.NotNil(t, m.before)
	for i := range m.editor.entries {
		if m.editor.entries[i].Key == "temperature" {
			m.editor.entries[i].Value = "0.7"
		}
	}
	m.Dirty = true
	return m, f
}

func TestParameterSaveOnlyChangesConfiguredLeaves(t *testing.T) {
	m, f := newParamsFixture(t)
	cmd := m.save()
	require.NotNil(t, cmd)
	assert.Nil(t, m.save(), "duplicate save must not dispatch")
	scope := m.scope
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, scope, m.scope, "a pending save keeps its scope")
	reply := cmd()
	_, reload := m.Update(reply)
	require.NotNil(t, reload)
	f.Lock()
	data, err := json.Marshal(f.patch)
	f.Unlock()
	require.NoError(t, err)
	assert.JSONEq(t, `{"models":{"model":{"parameters":{"temperature":0.7}}}}`, string(data))
	assert.False(t, m.Dirty)
	assert.False(t, m.pending)
	// A failed reread cannot turn a committed PATCH into a failed save.
	f.Lock()
	f.readFailure = true
	f.Unlock()
	m.Update(reload())
	assert.False(t, m.Dirty)
	assert.Equal(t, "0.7", m.before.Parameters["temperature"])
	assert.Contains(t, m.status, "reload")
}

func TestParameterSaveRefusesConcurrentEditedLeaf(t *testing.T) {
	m, f := newParamsFixture(t)
	f.Lock()
	f.temperature = "0.9"
	f.Unlock()
	m.Update(m.save()())
	assert.True(t, m.Dirty)
	assert.Contains(t, m.status, "another session")
	f.Lock()
	defer f.Unlock()
	assert.Nil(t, f.patch, "conflicted edits must not write")
}

func TestParameterSaveUncertainOutcomeBlocksResubmission(t *testing.T) {
	m, f := newParamsFixture(t)
	f.Lock()
	f.writeFailure = true
	f.Unlock()
	m.Update(m.save()())
	assert.True(t, m.Dirty)
	assert.True(t, m.uncertain)
	assert.False(t, m.pending)
	assert.Nil(t, m.save())
}

func TestParameterConflictsDistinguishAbsentEmptyAndDeleted(t *testing.T) {
	empty := ""
	before := &pkgClient.ConfiguredParameters{Parameters: map[string]string{"same": "1", "removed": "2", "empty": ""}}
	changes := pkgClient.ParameterChanges{Parameters: map[string]*string{"same": &empty, "removed": nil, "new": &empty}}
	fresh := &pkgClient.ConfiguredParameters{Parameters: map[string]string{"same": "1", "removed": "2", "unrelated": "3", "empty": "different"}}
	assert.Empty(t, parameterConflicts(before, fresh, changes))
	fresh.Parameters["new"] = ""
	delete(fresh.Parameters, "removed")
	assert.Equal(t, []string{"parameters.new", "parameters.removed"}, parameterConflicts(before, fresh, changes))
}

func TestParameterRepliesCannotCrossOwnerOrRefresh(t *testing.T) {
	m, _ := newParamsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := m.before
	m.Update(paramsLoadedMsg{ctx: ctx, scope: m.scope, before: &pkgClient.ConfiguredParameters{}})
	assert.Same(t, before, m.before)
	_ = m.save()
	m.Update(paramsSavedMsg{ctx: ctx})
	assert.True(t, m.pending)
}

func TestParameterOpenInputProtectsNavigation(t *testing.T) {
	m, _ := newParamsFixture(t)
	m.resetEditor()
	m.editor.list.SetCursor(1)
	m.editor.openForm(false)
	m.Update(tea.KeyPressMsg{Code: '9', Text: "9"})
	require.True(t, m.Dirty)
	root := tui.NewRoot(m)
	root.Update(tui.NavMenuMsg{})
	require.Same(t, m, root.Active())
	_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	require.True(t, m.editor.editing)
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, m.Dirty, "canceling the open field restores the untouched draft")
}

func TestParameterFailedScopeLoadCannotEditOldValues(t *testing.T) {
	m, _ := newParamsFixture(t)
	m.resetEditor()
	scope := pkgClient.ParameterScope{Scope: "node", Node: "worker"}
	_, cmd := m.Update(paramsScopeMsg{owner: m, scope: scope})
	require.NotNil(t, cmd)
	require.Nil(t, m.before)
	m.Update(paramsLoadedMsg{ctx: m.loadContext, scope: scope, err: errors.New("load failed")})
	m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	require.Nil(t, m.before)
	require.Nil(t, m.save())
	require.False(t, m.pending)
}

func TestParameterReloadCapturesSelectedScope(t *testing.T) {
	m, _ := newParamsFixture(t)
	m.resetEditor()
	before := m.scope
	cmd, handled := m.updateScopeKey(tea.KeyPressMsg{Code: 'r', Text: "r"})
	require.True(t, handled)
	m.scope = pkgClient.ParameterScope{Scope: "node", Node: "other"}
	msg := cmd().(paramsScopeMsg)
	require.Equal(t, before, msg.scope)
}
