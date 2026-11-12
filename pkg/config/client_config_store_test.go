package config

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func clientStoreFixture(t *testing.T) *ConfigManager {
	t.Helper()
	root := t.TempDir()
	t.Setenv("ZZROUTER_TEST_HOME", root)
	cm := NewConfigManager("zzrouter")
	require.Equal(t, root, cm.GetClientConfigDir(), "test override must isolate client files too")
	return cm
}

func TestClientConfigStoresMergeConcurrentFields(t *testing.T) {
	cm := clientStoreFixture(t)
	var stores []*ClientConfigStore
	for range 5 {
		store, err := NewClientConfigStore(cm)
		require.NoError(t, err)
		stores = append(stores, store)
	}
	before := stores[0].Config()
	beforeSearch := before.Preferences.Search.DefaultRegistry
	updates := []func() error{
		func() error { return stores[0].SetSearchPrefs("huggingface", "likes", []string{"gguf"}) },
		func() error { return stores[1].SetModelTags([]TagFilter{{Tags: []string{"vision"}, Logic: "OR"}}) },
		func() error {
			return stores[2].SetChatSettings(ChatSettings{MaxTokens: 321, Temperature: 0.8, TopP: 0.9})
		},
		func() error { return stores[3].SetTheme("tokyo-night") },
		func() error { return stores[4].SetNodeConnection("fixture.invalid", 8443, true) },
	}
	errors := make(chan error, len(updates))
	var wg sync.WaitGroup
	for _, update := range updates {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- update() }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	cfg, err := cm.LoadClientConfig()
	require.NoError(t, err)
	assert.Equal(t, SearchRegistryPrefs{Sort: "likes", Tags: []string{"gguf"}}, cfg.Preferences.Search.RegistryPrefs["huggingface"])
	assert.Equal(t, []TagFilter{{Tags: []string{"vision"}, Logic: "OR"}}, cfg.Preferences.Search.ModelTags)
	assert.Equal(t, "tokyo-night", cfg.Preferences.Theme)
	assert.Equal(t, 321, cfg.Preferences.Chat.MaxTokens)
	assert.Equal(t, 0.8, cfg.Preferences.Chat.Temperature)
	assert.Equal(t, "fixture.invalid", cfg.Node.Address)
	assert.Equal(t, 8443, cfg.Node.Port)
	assert.True(t, cfg.Node.Secure)
	assert.Equal(t, beforeSearch, before.Preferences.Search.DefaultRegistry, "previous Config snapshots stay immutable")
	assert.Equal(t, before.Preferences.Timeout, cfg.Preferences.Timeout, "unchanged template defaults survive each merge")
}

func TestClientConfigStoreOwnsMutableInputs(t *testing.T) {
	cm := clientStoreFixture(t)
	store, err := NewClientConfigStore(cm)
	require.NoError(t, err)
	tags := []string{"gguf"}
	require.NoError(t, store.SetSearchPrefs("huggingface", "downloads", tags))
	snapshot := store.Config()
	tags[0] = "mutated"
	assert.Equal(t, []string{"gguf"}, snapshot.Preferences.Search.RegistryPrefs["huggingface"].Tags)
	presets := []TagFilter{{Tags: []string{"vision"}, Logic: "AND"}}
	require.NoError(t, store.SetModelTags(presets))
	presets[0].Tags[0] = "mutated"
	assert.Equal(t, "vision", store.Config().Preferences.Search.ModelTags[0].Tags[0])
	assert.Equal(t, "downloads", snapshot.Preferences.Search.RegistryPrefs["huggingface"].Sort)
}

func TestClientConfigStoreReturnsReloadFailureWithoutChangingSnapshot(t *testing.T) {
	cm := clientStoreFixture(t)
	store, err := NewClientConfigStore(cm)
	require.NoError(t, err)
	require.NoError(t, store.SetTheme("tokyo-night"))
	before := store.Config()
	require.NoError(t, os.WriteFile(cm.GetClientConfigPath(), []byte("preferences: [invalid"), 0o600))
	err = store.SetSearchPrefs("huggingface", "likes", nil)
	require.ErrorContains(t, err, "reload client config")
	assert.Same(t, before, store.Config())
	contents, err := os.ReadFile(cm.GetClientConfigPath())
	require.NoError(t, err)
	assert.Equal(t, "preferences: [invalid", string(contents), "failed mutation must not overwrite the unreadable file")
	require.NoError(t, cm.SaveClientConfig(before))
	require.NoError(t, store.SetSearchPrefs("huggingface", "likes", nil))
	assert.Equal(t, "tokyo-night", store.Config().Preferences.Theme)
}

func TestClientConfigSavePublishesCompleteSnapshots(t *testing.T) {
	cm := clientStoreFixture(t)
	first := &ClientConfig{Node: NodeConnection{Address: "first"}, Preferences: ClientPreferences{Chat: ChatSettings{SystemPrompt: strings.Repeat("a", 8192)}}}
	second := &ClientConfig{Node: NodeConnection{Address: "second"}, Preferences: ClientPreferences{Chat: ChatSettings{SystemPrompt: strings.Repeat("b", 8192)}}}
	require.NoError(t, cm.SaveClientConfig(first))
	stop := make(chan struct{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			data, err := os.ReadFile(cm.GetClientConfigPath())
			if err != nil {
				done <- err
				return
			}
			var got ClientConfig
			if err := yaml.Unmarshal(data, &got); err != nil {
				done <- err
				return
			}
			want := first
			if got.Node.Address == "second" {
				want = second
			}
			if got.Node.Address != want.Node.Address || got.Preferences.Chat.SystemPrompt != want.Preferences.Chat.SystemPrompt {
				done <- errors.New("reader observed incomplete client configuration")
				return
			}
		}
	}()
	<-started
	for i := range 8 {
		cfg := first
		if i%2 == 0 {
			cfg = second
		}
		assert.NoError(t, cm.SaveClientConfig(cfg))
	}
	close(stop)
	require.NoError(t, <-done)
}
