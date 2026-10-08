package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/config/templates"
	"github.com/stperic/zzrouter/pkg/constants"
)

// TestAddProviderInstance_OllamaConnectRoundTrip is the critical contract
// test for the runtime-provider-instance surface. It exercises the full
// create → persist → reload → read loop: build an ExternalProvider via the
// ollama-connect factory, push it through the store, write it to disk via
// SaveToDir, load a fresh AppsConfig from the same directory, and assert
// every field that matters for dispatch survived the typed→yaml→typed
// round-trip intact.
//
// This pins three things that would otherwise break silently:
//  1. ExternalProvider.Defaults is nil on factory output — the yaml marshal
//     path must accept that AND the loader must round-trip it.
//  2. providerToServiceConfig → AddApp → serviceConfigToProvider converter
//     symmetry preserves Protocol, Runtime.Endpoint, and Runtime.API.Token.
//  3. The file lands under external/<name>.yaml, matching the kind
//     subdirectory layout the loader walks.
func TestAddProviderInstance_OllamaConnectRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	// Factory builds a connect instance with a bearer token so we can
	// assert auth config survives persistence.
	p := NewOllamaConnectProvider("ollama-nas", "http://nas.lan:11434", "sekret")
	require.NoError(t, store.AddProviderInstance("ollama-nas", p))

	// First: the in-memory store observes the new entry.
	sc, ok := store.Config().LookupApp("ollama-nas")
	require.True(t, ok, "entry missing from in-memory config")
	assert.Equal(t, constants.AppModeExternal, sc.Mode)
	assert.Equal(t, ProtocolOllama, sc.Protocol)
	assert.Equal(t, "http://nas.lan:11434", sc.Runtime.Endpoint)
	require.NotNil(t, sc.Runtime.API, "API config dropped on conversion")
	assert.Equal(t, "sekret", sc.Runtime.API.Token)

	// Second: a fresh store loaded from the same directory sees the entry.
	// This is the round-trip that catches yaml marshal/unmarshal drops.
	fresh, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	got := fresh.Config().GetExternal("ollama-nas")
	require.NotNil(t, got, "entry missing after reload from disk")
	assert.Equal(t, ProtocolOllama, got.Protocol)
	assert.Equal(t, "http://nas.lan:11434", got.Runtime.Endpoint)
	require.NotNil(t, got.Enabled)
	assert.True(t, *got.Enabled)
	require.NotNil(t, got.Runtime.API, "API config dropped across yaml round-trip")
	assert.Equal(t, "bearer", got.Runtime.API.AuthType)
	assert.Equal(t, "sekret", got.Runtime.API.Token)

	// The shipped managed ollama entry is untouched.
	shipped := fresh.Config().GetExternal("ollama")
	require.NotNil(t, shipped, "shipped ollama entry clobbered")
	assert.NotEqual(t, "http://nas.lan:11434", shipped.Runtime.Endpoint)
}

// TestAddProviderInstance_RejectsDuplicate pins the typed ErrProviderExists
// sentinel so callers can use errors.Is rather than substring matching.
func TestAddProviderInstance_RejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	p := NewOllamaConnectProvider("ollama-nas", "http://nas.lan:11434", "")
	require.NoError(t, store.AddProviderInstance("ollama-nas", p))

	err = store.AddProviderInstance("ollama-nas", p)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderExists),
		"collision error must wrap ErrProviderExists; got %v", err)

	// Colliding with a shipped provider name also triggers the sentinel.
	p2 := NewOllamaConnectProvider("ollama", "http://other:11434", "")
	err = store.AddProviderInstance("ollama", p2)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrProviderExists),
		"collision with shipped provider must wrap ErrProviderExists; got %v", err)
}

// TestAddProviderInstance_NoAuthSendsNothing confirms that a connect
// instance created without a token declares that the daemon gets no
// credential (neither a bearer header nor the caller's key), and that the
// declaration survives the yaml round-trip.
func TestAddProviderInstance_NoAuthSendsNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	p := NewOllamaConnectProvider("ollama-lan", "http://10.0.0.5:11434", "")
	require.NoError(t, store.AddProviderInstance("ollama-lan", p))

	fresh, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	got := fresh.Config().GetExternal("ollama-lan")
	require.NotNil(t, got)
	require.NotNil(t, got.Runtime.API, "an absent api block would forward the caller's key")
	assert.Equal(t, AuthTypeNone, got.Runtime.API.AuthType)
	assert.Empty(t, got.Runtime.API.Token)
	assert.Equal(t, "http://10.0.0.5:11434", got.Runtime.Endpoint)
}

// TestListenerPanicDoesNotBreakChain pins the invariant documented on
// AppsConfigStore.notify: a panicking listener logs and is skipped, but
// listeners that come after it still observe the new config. The
// listener order is meaningful at the server level (provider manager
// before model registry before local cache refresh), and a panic inside
// a shell-out-based reloader must NOT leave the downstream subsystems
// unaware of a mutation the store already persisted.
//
// Without the recovery barrier, a panic here would unwind up through
// mutate() and SetProviderEnabled, and the caller would see a 500 while
// the on-disk config is already flipped — exactly the half-reconciled
// state the FinalizeOnboarding contract promises to eliminate.
func TestListenerPanicDoesNotBreakChain(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	var firstRan, secondRan bool
	store.OnChange("panicker", func(*AppsConfig) ReloadDisposition {
		firstRan = true
		panic("simulated reloader crash (e.g., version-detector shell-out)")
	})
	store.OnChange("follower", func(*AppsConfig) ReloadDisposition {
		secondRan = true
		return DispositionApplied
	})

	// Trigger a real mutation via a known-enabled provider from the
	// shipped templates. SetProviderEnabled is idempotent; if the
	// provider starts disabled we still force a change by flipping on.
	require.NoError(t, store.SetProviderEnabled("vllm", true),
		"mutation must return nil even when a listener panics")

	assert.True(t, firstRan, "first (panicking) listener was called")
	assert.True(t, secondRan,
		"second listener MUST run — a panic in listener N must not "+
			"skip listeners N+1..K; downstream subsystems would desync")
}

// TestAddCloudModel_RoundTrip pins that registered cloud models survive
// the typed→yaml→typed round-trip. CloudProvider previously lacked a
// Capabilities field, so AddCloudModel returned success but the model
// was silently dropped on the first reload — /v1/models then 404'd on
// inference. Regression guard for the openrouter blocker.
func TestAddCloudModel_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	const modelID = "meta-llama/llama-3.2-3b-instruct"
	require.NoError(t, store.AddCloudModel("openrouter", modelID))

	// In-memory: the model is registered.
	cp := store.Config().GetCloud("openrouter")
	require.NotNil(t, cp, "openrouter cloud provider missing")
	require.NotNil(t, cp.Capabilities, "Capabilities nil after AddCloudModel")
	assert.Contains(t, cp.Capabilities.Models, modelID)

	// Fresh load from disk: the model survives the yaml round-trip.
	fresh, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	got := fresh.Config().GetCloud("openrouter")
	require.NotNil(t, got, "openrouter missing after reload")
	require.NotNil(t, got.Capabilities, "Capabilities dropped on yaml round-trip")
	assert.Contains(t, got.Capabilities.Models, modelID,
		"registered cloud model dropped by CloudProvider yaml round-trip")

	// Removal also persists.
	require.NoError(t, fresh.RemoveCloudModels(map[string][]string{
		"openrouter": {modelID},
	}))
	final, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	if cap := final.Config().GetCloud("openrouter").Capabilities; cap != nil {
		assert.NotContains(t, cap.Models, modelID, "removed model resurfaced after reload")
	}
}

// TestSetProviderPinnedVersion_RoundTrip pins that a version pin survives
// the typed→yaml→typed round-trip and that clearing it removes the pin.
// The pin is what install/upgrade resolve against when no explicit version
// is supplied, so a pin that silently failed to persist would send every
// later install back to the previous release.
func TestSetProviderPinnedVersion_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	require.NoError(t, store.SetProviderPinnedVersion("llamacpp", "b10453"))

	reloaded, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	svc, ok := reloaded.Config().LookupApp("llamacpp")
	require.True(t, ok)
	assert.Equal(t, "b10453", svc.PinnedVersion, "pin must survive reload")

	// Empty clears the pin, restoring "resolve latest upstream".
	require.NoError(t, store.SetProviderPinnedVersion("llamacpp", ""))
	cleared, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	svc, ok = cleared.Config().LookupApp("llamacpp")
	require.True(t, ok)
	assert.Empty(t, svc.PinnedVersion, "empty version must clear the pin")
}

// TestSetProviderPinnedVersion_UnknownProvider guards the error path so a
// typo returns ErrProviderNotFound rather than silently succeeding.
func TestSetProviderPinnedVersion_UnknownProvider(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))

	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	err = store.SetProviderPinnedVersion("does-not-exist", "b1")
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

// Assets is the provider's own directory under its kind, and only a
// loaded provider has one.
func TestAssetsIsTheProvidersAssetDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	d, err := store.Assets("llamacpp")
	require.NoError(t, err)
	_, err = d.Write("x.jinja", []byte("x"))
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(dir, "on-demand", "llamacpp", "assets", "x.jinja"))
	assert.NoError(t, statErr)

	_, err = store.Assets("no-such-provider")
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

// An asset write or delete is a change to the provider's definition, so
// listeners hear of it as they hear of a config mutation; that is what
// carries it to workers. A refused write changes nothing and says nothing.
func TestAssetMutationsNotifyListeners(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	var notified int
	store.OnChange("counter", func(*AppsConfig) ReloadDisposition {
		notified++
		return DispositionApplied
	})

	created, err := store.WriteAsset("llamacpp", "t.jinja", []byte("x"))
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 1, notified)

	_, err = store.WriteAsset("llamacpp", "../escape", []byte("x"))
	require.ErrorIs(t, err, assets.ErrInvalidName)
	require.ErrorIs(t, store.DeleteAsset("llamacpp", "absent", noRefs), assets.ErrNotFound)
	assert.Equal(t, 1, notified, "a refused mutation notifies nobody")

	require.NoError(t, store.DeleteAsset("llamacpp", "t.jinja", noRefs))
	assert.Equal(t, 2, notified)

	_, err = store.WriteAsset("no-such-provider", "t.jinja", []byte("x"))
	assert.ErrorIs(t, err, ErrProviderNotFound)
}

func noRefs(ServiceConfig) []string { return nil }

// The in-use check runs under the store's lock, against the config the
// store holds, and a refusal removes nothing.
func TestDeleteAssetRefusesWhileInUse(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	_, err = store.WriteAsset("llamacpp", "t.jinja", []byte("x"))
	require.NoError(t, err)

	var seen string
	err = store.DeleteAsset("llamacpp", "t.jinja", func(cfg ServiceConfig) []string {
		seen = cfg.Name
		return []string{"defaults.parameters.k"}
	})
	var inUse *AssetInUseError
	require.ErrorAs(t, err, &inUse)
	assert.Equal(t, []string{"defaults.parameters.k"}, inUse.Paths)
	assert.NotEmpty(t, seen)
	d, err := store.Assets("llamacpp")
	require.NoError(t, err)
	_, err = d.Path("t.jinja")
	assert.NoError(t, err, "a refused delete removes nothing")

	err = store.DeleteAsset("llamacpp", "absent", func(ServiceConfig) []string { t.Fatal("no check for an absent asset"); return nil })
	assert.ErrorIs(t, err, assets.ErrNotFound)
}

// Mutators edit the provider map in place, so finding a provider's
// directory must read it under the lock. Run with -race to see it.
func TestProviderDir_ReadsUnderTheLock(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)

	const writes = 20
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range writes {
			name := fmt.Sprintf("ollama-%d", i)
			assert.NoError(t, store.AddProviderInstance(name, NewOllamaConnectProvider(name, "http://127.0.0.1:11434", "")))
		}
	}()
	for range writes {
		_, err := store.Assets("llamacpp")
		assert.NoError(t, err)
	}
	wg.Wait()
}

// A refused mutation leaves the stored provider exactly as it was: the
// mutator is handed a copy, not the provider's own maps.
func TestUpdateApp_RefusedMutationLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, templates.InstallDefaults(dir))
	store, err := NewAppsConfigStore(dir)
	require.NoError(t, err)
	before, ok := store.Config().LookupApp("llamacpp")
	require.True(t, ok)
	ctxSize := before.Defaults.Parameters["ctx-size"]
	require.NotEqual(t, "1", ctxSize)

	refused := errors.New("refused after editing")
	err = store.ApplyParameterPatch("llamacpp", func(sc *ServiceConfig) error {
		sc.Defaults.Parameters["ctx-size"] = "1"
		if sc.Models == nil {
			sc.Models = map[string]ModelSpec{}
		}
		sc.Models["ghost"] = ModelSpec{Parameters: map[string]string{"k": "v"}}
		return refused
	})
	require.ErrorIs(t, err, refused)

	after, ok := store.Config().LookupApp("llamacpp")
	require.True(t, ok)
	assert.Equal(t, ctxSize, after.Defaults.Parameters["ctx-size"])
	assert.NotContains(t, after.Models, "ghost")
}
