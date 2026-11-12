package search

import (
	"errors"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func preferenceTestConfig(t *testing.T) *pkgConfig.ConfigManager {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	cm := pkgConfig.NewConfigManager("zzrouter")
	require.Equal(t, root, cm.GetClientConfigDir())
	require.NoError(t, cm.SaveClientConfig(&pkgConfig.ClientConfig{
		Node: pkgConfig.NodeConnection{Address: "fixture.invalid", Port: 8443, Secure: true},
		Preferences: pkgConfig.ClientPreferences{
			Theme: "catppuccin-mocha", Chat: pkgConfig.ChatSettings{MaxTokens: 1234},
			Search: pkgConfig.SearchSettings{DefaultLogic: "OR"},
		},
	}))
	return cm
}

func TestSearchPreferenceCommandsPreserveInputOrderAndFields(t *testing.T) {
	cm := preferenceTestConfig(t)
	m := activeSearch(t, &discoveryFixture{})
	m.sortBy, m.tagFilter = "downloads", []string{"gguf"}
	m.queueSearchPreferences()
	m.tagPresets = []TagPreset{{Tags: []string{"vision"}, Logic: "AND"}}
	m.queueTagPreferences()
	m.tagPresets[0].Tags[0] = "changed-after-enqueue"
	m.sortBy = "likes"
	m.queueSearchPreferences()
	m.tagFilter[0] = "changed-after-enqueue"
	m.Provider, m.sortBy, m.tagFilter = "openai", "name", nil
	m.queueSearchPreferences()

	// The newer event's command can run first, even after the pane is left.
	// Every command drains the same immutable queue in UI event order.
	m.Deactivate()
	for i := len(m.preferenceCommands) - 1; i >= 0; i-- {
		_ = m.preferenceCommands[i]()
	}
	cfg, err := cm.LoadClientConfig()
	require.NoError(t, err)
	assert.Equal(t, "likes", cfg.Preferences.Search.RegistryPrefs["huggingface"].Sort)
	assert.Equal(t, []string{"gguf"}, cfg.Preferences.Search.RegistryPrefs["huggingface"].Tags)
	assert.Equal(t, "openai", cfg.Preferences.Search.DefaultRegistry)
	assert.Equal(t, "name", cfg.Preferences.Search.RegistryPrefs["openai"].Sort)
	assert.Equal(t, []pkgConfig.TagFilter{{Tags: []string{"vision"}, Logic: "AND"}}, cfg.Preferences.Search.ModelTags)
	assert.Equal(t, "OR", cfg.Preferences.Search.DefaultLogic)
	assert.Equal(t, "catppuccin-mocha", cfg.Preferences.Theme)
	assert.Equal(t, 1234, cfg.Preferences.Chat.MaxTokens)
	assert.Equal(t, "fixture.invalid", cfg.Node.Address)
	assert.Equal(t, 8443, cfg.Node.Port)
	assert.True(t, cfg.Node.Secure)
}

func TestSearchPreferenceCommandsAcrossUpdatesCanStartConcurrently(t *testing.T) {
	cm := preferenceTestConfig(t)
	m := activeSearch(t, &discoveryFixture{})
	m.results = []searchResult{{Name: "A"}}
	m.providerConfigs = map[string]searchProviderConfig{"huggingface": {ClientSorts: map[string]bool{"downloads": true, "likes": true}}}
	var commands []tea.Cmd
	for i := range 8 {
		m.State, m.prevState = statePickerOverlay, StateList
		m.pickerAction, m.pickerKeys = "sort", []string{"downloads", "likes"}
		m.pickerCursor = i % 2
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		require.NotNil(t, cmd)
		commands = append(commands, cmd)
	}
	m.Deactivate()
	_ = m.Activate() // Reentry keeps the same preference writer.
	var wg sync.WaitGroup
	for i := len(commands) - 1; i >= 0; i-- {
		wg.Add(1)
		go func(cmd tea.Cmd) {
			defer wg.Done()
			_ = cmd()
		}(commands[i])
	}
	wg.Wait()
	cfg, err := cm.LoadClientConfig()
	require.NoError(t, err)
	assert.Equal(t, "likes", cfg.Preferences.Search.RegistryPrefs["huggingface"].Sort)
}

type preferenceStoreFixture struct {
	search func(string, string, []string) error
	tags   func([]pkgConfig.TagFilter) error
}

func (f preferenceStoreFixture) SetSearchPrefs(registry, sort string, tags []string) error {
	return f.search(registry, sort, tags)
}

func (f preferenceStoreFixture) SetModelTags(tags []pkgConfig.TagFilter) error {
	return f.tags(tags)
}

func TestPreferenceWriterKeepsFailedChangesWithoutBlockingEnqueue(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	failed := errors.New("fixture disk unavailable")
	var calls []string
	m := activeSearch(t, &discoveryFixture{})
	m.preferenceWriter.store = preferenceStoreFixture{
		search: func(_, sort string, _ []string) error {
			calls = append(calls, sort)
			if len(calls) == 1 {
				close(started)
				<-release
				return failed
			}
			return nil
		},
		tags: func([]pkgConfig.TagFilter) error { calls = append(calls, "tags"); return nil },
	}
	m.sortBy = "downloads"
	m.queueSearchPreferences()
	assert.Empty(t, calls, "queueing performs no persistence")
	done := make(chan error, 1)
	go func() { done <- m.preferenceWriter.flush() }()
	<-started
	enqueued := make(chan struct{})
	go func() {
		m.tagPresets = []TagPreset{{Tags: []string{"gguf"}, Logic: "AND"}}
		m.queueTagPreferences()
		m.sortBy = "likes"
		m.queueSearchPreferences()
		close(enqueued)
	}()
	select {
	case <-enqueued:
	case <-time.After(2 * time.Second):
		unblock()
		<-enqueued
		t.Fatal("Update's enqueue path waited for disk I/O")
	}
	m.Deactivate()
	unblock()
	require.ErrorIs(t, <-done, failed)
	require.NoError(t, m.preferenceWriter.flush())
	assert.Equal(t, []string{"downloads", "downloads", "tags", "likes"}, calls)
}
