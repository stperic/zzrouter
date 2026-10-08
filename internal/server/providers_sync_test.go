package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
)

// newFanOutTestStore builds a real AppsConfigStore backed by a tempdir
// with one ollama provider; keeps the test close to production.
func newFanOutTestStore(t *testing.T) *pkgConfig.AppsConfigStore {
	t.Helper()
	dir := t.TempDir()
	external := filepath.Join(dir, "external", "ollama")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := []byte("description: test\nenabled: true\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:11434\ncapabilities:\n  wire_endpoints: [chat_completions]\n")
	if err := os.WriteFile(filepath.Join(external, "config.yaml"), body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := pkgConfig.NewAppsConfigStore(dir)
	if err != nil {
		t.Fatalf("NewAppsConfigStore: %v", err)
	}
	return store
}

func TestProviderSyncFanOut_IgnoredWhenNotCoord(t *testing.T) {
	store := newFanOutTestStore(t)
	f := newProviderSyncFanOut(
		store,
		func() bool { return false }, // not coord
		func() []*mesh.Endpoint { return nil },
		func() *http.Client { return http.DefaultClient },
	)
	disp := f.Listener()(store.Config())
	if disp != pkgConfig.DispositionIgnored {
		t.Errorf("non-coord listener disposition = %v, want Ignored", disp)
	}
}

func TestProviderSyncFanOut_PushesToEveryWorker(t *testing.T) {
	store := newFanOutTestStore(t)

	type received struct {
		path string
		body ProviderSyncBody
	}
	recv := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body ProviderSyncBody
		_ = json.Unmarshal(raw, &body)
		recv <- received{path: r.URL.Path, body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}}
		},
		func() *http.Client { return srv.Client() },
	)
	disp := f.Listener()(store.Config())
	if disp != pkgConfig.DispositionApplied {
		t.Errorf("coord listener disposition = %v, want Applied", disp)
	}
	select {
	case got := <-recv:
		if !bytes.HasSuffix([]byte(got.path), []byte("/sync/providers/ollama")) {
			t.Errorf("path = %q, want suffix /sync/providers/ollama", got.path)
		}
		if len(got.body.ConfigYAML) == 0 {
			t.Error("config_yaml body empty")
		}
		if got.body.Kind != pkgConfig.KindExternal {
			t.Errorf("kind = %q, want external", got.body.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("no push received within 1s")
	}
}

func TestProviderSyncFanOut_CoalescesBursts(t *testing.T) {
	// M2: rapid back-to-back Listener invocations during an in-flight
	// run must coalesce into at most one re-fire, not per-invocation
	// runs. With N providers = 1 × N workers = 1, pure coalesce bounds
	// the push count at 2 (initial + one re-fire) regardless of burst
	// size.
	store := newFanOutTestStore(t)
	var hits int64
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)
	// First fire enters run(); subsequent fires during the block coalesce.
	f.Listener()(store.Config())
	for i := 0; i < 20; i++ {
		f.Listener()(store.Config())
	}
	// Release the in-flight push. Re-fire (if any) executes next.
	close(block)
	// Allow re-fire to run.
	time.Sleep(100 * time.Millisecond)
	got := atomic.LoadInt64(&hits)
	if got < 1 || got > 2 {
		t.Errorf("coalesce bound violated: hits=%d, want 1 or 2", got)
	}
}

func TestProviderSyncFanOut_SkipsCloudProviders(t *testing.T) {
	// Cloud + registry configs carry API keys in defaults.environment and
	// run only on the coordinator — they must never land in worker caches.
	dir := t.TempDir()
	for _, d := range []string{
		filepath.Join(dir, "external", "ollama"),
		filepath.Join(dir, "cloud", "openai"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	extBody := []byte("protocol: ollama\nruntime:\n  endpoint: http://localhost:11434\ncapabilities:\n  wire_endpoints: [chat_completions]\n")
	cloudBody := []byte("runtime:\n  endpoint: https://api.openai.com\n  model_url: x\n  api:\n    auth_type: bearer\n    token: sk-SECRET\ncapabilities:\n  wire_endpoints: [chat_completions]\n")
	if err := os.WriteFile(filepath.Join(dir, "external", "ollama", "config.yaml"), extBody, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cloud", "openai", "config.yaml"), cloudBody, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := pkgConfig.NewAppsConfigStore(dir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	type received struct{ path string }
	recv := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv <- received{path: r.URL.Path}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)
	_ = f.Listener()(store.Config())

	done := time.After(500 * time.Millisecond)
	var paths []string
loop:
	for {
		select {
		case r := <-recv:
			paths = append(paths, r.path)
		case <-done:
			break loop
		}
	}
	if len(paths) != 1 {
		t.Fatalf("expected exactly one push (ollama); got %d paths: %v", len(paths), paths)
	}
	if !bytes.Contains([]byte(paths[0]), []byte("ollama")) {
		t.Errorf("path = %q, want suffix for ollama", paths[0])
	}
	for _, p := range paths {
		if bytes.Contains([]byte(p), []byte("openai")) {
			t.Errorf("cloud provider openai leaked to worker: %q", p)
		}
	}
}

func TestProviderSyncFanOut_SkipsLocalEndpoint(t *testing.T) {
	store := newFanOutTestStore(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{
				{URL: srv.URL, NodeName: "self", IsLocal: true},
			}
		},
		func() *http.Client { return srv.Client() },
	)
	_ = f.Listener()(store.Config())
	// Give run() a brief window; no push should fire.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("pushes to local endpoint = %d, want 0", got)
	}
}

func TestHandleProviderSync_WritesAndReloads(t *testing.T) {
	store := newFanOutTestStore(t)
	body := ProviderSyncBody{
		Kind:       pkgConfig.KindExternal,
		ConfigYAML: []byte("description: sync-rewritten\nenabled: true\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:22222\ncapabilities:\n  wire_endpoints: [chat_completions]\n"),
	}
	raw, _ := json.Marshal(body)

	if _, err := store.WriteProviderFiles("ollama", body.Kind, pkgConfig.ProviderFiles{Config: body.ConfigYAML, Schema: body.SchemaYAML, Assets: body.Assets}); err != nil {
		t.Fatalf("WriteProviderFiles: %v", err)
	}

	cfg := store.Config()
	sc, ok := cfg.LookupApp("ollama")
	if !ok || sc.Description != "sync-rewritten" {
		t.Fatalf("post-sync description = %q, want sync-rewritten", sc.Description)
	}

	// Also exercise the bytes path: a raw POST would marshal/unmarshal
	// through the same shape, so confirm the wire contract holds.
	var decoded ProviderSyncBody
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded.ConfigYAML, body.ConfigYAML) {
		t.Error("round-trip config bytes mismatch")
	}
}

// TestDecodeProviderBytes_RejectsInvalidWireEndpoints — pre-write
// validation must catch a config that fails Validate() (e.g. mutually
// exclusive responses + responses_compat) BEFORE the bytes touch disk,
// so a buggy or compromised coord can't push a config that bricks the
// worker's next reload. Exercised via the public DecodeProviderBytes
// helper that HandleProviderSync calls; the handler is mounted on the
// mTLS cluster-port listener (not s.engine) so direct HTTP testing
// would require a much heavier harness.
func TestDecodeProviderBytes_RejectsInvalidWireEndpoints(t *testing.T) {
	bad := []byte("description: bad\nenabled: true\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:11434\ncapabilities:\n  wire_endpoints: [chat_completions, responses, responses_compat]\n")
	p, err := pkgConfig.DecodeProviderBytes(pkgConfig.KindExternal, "badprov", bad)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() accepted responses + responses_compat; want mutual-exclusion error")
	} else if !bytes.Contains([]byte(err.Error()), []byte("mutually exclusive")) {
		t.Errorf("Validate() error = %q; want mutually-exclusive reason", err)
	}
}

// TestDecodeProviderBytes_RejectsMissingWireEndpoints — required-field
// validation must trip too. wire_endpoints is required at load; the
// receiver must fail closed at the wire on configs that omit it
// rather than land bytes that break the next reload.
func TestDecodeProviderBytes_RejectsMissingWireEndpoints(t *testing.T) {
	bad := []byte("description: missing\nenabled: true\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:11434\n")
	p, err := pkgConfig.DecodeProviderBytes(pkgConfig.KindExternal, "missingprov", bad)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate() accepted missing wire_endpoints; want required-field error")
	}
}

// TestDecodeProviderBytes_AcceptsDisabledWithoutWireEndpoints — disabled
// providers don't have to carry a capabilities block; the load-time
// wire_endpoints gate only fires for !IsExplicitlyDisabled. Pinning this
// so a future "be strict everywhere" refactor doesn't reintroduce the
// disabled-cloud-needs-capabilities footgun.
func TestDecodeProviderBytes_AcceptsDisabledWithoutWireEndpoints(t *testing.T) {
	disabled := []byte("description: off\nenabled: false\nprotocol: ollama\nruntime:\n  endpoint: http://localhost:11434\n")
	p, err := pkgConfig.DecodeProviderBytes(pkgConfig.KindExternal, "offprov", disabled)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() rejected disabled provider with no wire_endpoints: %v", err)
	}
}

// The receiving end must be idempotent, because that is what makes the
// scheduled push affordable. A worker already in step writes nothing
// and, more importantly, reloads nothing: the listener chain behind a
// reload rebuilds the provider manager, the model registry and the
// model cache, and doing that on every node every tick would be a
// self-inflicted load with no change to show for it.
func TestWriteProviderFiles_IdenticalBytesChangeNothing(t *testing.T) {
	store := newFanOutTestStore(t)
	body, err := store.ReadProviderConfigBytes("ollama")
	if err != nil {
		t.Fatalf("ReadProviderConfigBytes: %v", err)
	}

	var reloads int
	store.OnChange("counter", func(*pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
		reloads++
		return pkgConfig.DispositionApplied
	})

	changed, err := store.WriteProviderFiles("ollama", pkgConfig.KindExternal, pkgConfig.ProviderFiles{Config: body})
	if err != nil {
		t.Fatalf("re-write identical bytes: %v", err)
	}
	if changed {
		t.Error("identical bytes reported as changed")
	}
	if reloads != 0 {
		t.Errorf("identical bytes fired %d reload(s), want 0", reloads)
	}

	// A real difference must still land, or the comparison has turned
	// the sync into a no-op and the drift it exists to repair survives.
	edited := append(append([]byte{}, body...), []byte("\n# drift\n")...)
	changed, err = store.WriteProviderFiles("ollama", pkgConfig.KindExternal, pkgConfig.ProviderFiles{Config: edited})
	if err != nil {
		t.Fatalf("write changed bytes: %v", err)
	}
	if !changed {
		t.Error("changed bytes reported as unchanged")
	}
	if reloads != 1 {
		t.Errorf("changed bytes fired %d reload(s), want 1", reloads)
	}
	after, err := store.ReadProviderConfigBytes("ollama")
	if err != nil || !bytes.Equal(after, edited) {
		t.Errorf("on-disk bytes did not follow the write (err=%v)", err)
	}
}

// The whole point of the reconcile path: a push that no mutation
// triggered. If this only fired on OnChange, a worker that was
// unreachable during the one event that mattered stays wrong forever.
func TestReconcileProviderTree_PushesWithoutAMutation(t *testing.T) {
	store := newFanOutTestStore(t)

	pushes := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &Server{configStore: store}
	s.providers.syncFanOut = newProviderSyncFanOut(
		store,
		func() bool { return true },
		func() []*mesh.Endpoint {
			return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}}
		},
		func() *http.Client { return srv.Client() },
	)

	s.reconcileProviderTree("test")

	select {
	case path := <-pushes:
		if !strings.HasSuffix(path, "/sync/providers/ollama") {
			t.Errorf("path = %q, want suffix /sync/providers/ollama", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile pushed nothing within 2s")
	}
}

// Reconcile must stay silent where the fan-out would be wrong: a worker
// pushing its own tree at its peers, or a node with nothing wired.
func TestReconcileProviderTree_QuietWhenItShouldBe(t *testing.T) {
	store := newFanOutTestStore(t)
	pushed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pushed <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Not a coordinator.
	s := &Server{configStore: store}
	s.providers.syncFanOut = newProviderSyncFanOut(
		store,
		func() bool { return false },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() },
	)
	s.reconcileProviderTree("test")

	// Nothing wired at all: must not panic.
	(&Server{}).reconcileProviderTree("test")

	select {
	case <-pushed:
		t.Error("a non-coordinator pushed its provider tree at a peer")
	case <-time.After(200 * time.Millisecond):
	}
}

// The push carries the provider's whole asset set, so a worker can
// resolve an asset name at launch from its own disk.
func TestProviderSyncFanOut_CarriesAssets(t *testing.T) {
	store := newFanOutTestStore(t)
	_, err := store.WriteAsset("ollama", "t.jinja", []byte("{{ messages }}"))
	if err != nil {
		t.Fatalf("WriteAsset: %v", err)
	}

	recv := make(chan ProviderSyncBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body ProviderSyncBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		recv <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newProviderSyncFanOut(store, func() bool { return true },
		func() []*mesh.Endpoint { return []*mesh.Endpoint{{URL: srv.URL, NodeName: "w1"}} },
		func() *http.Client { return srv.Client() })
	f.Listener()(store.Config())

	select {
	case got := <-recv:
		if string(got.Assets["t.jinja"]) != "{{ messages }}" || len(got.Assets) != 1 {
			t.Errorf("assets = %v, want exactly t.jinja", got.Assets)
		}
	case <-time.After(time.Second):
		t.Fatal("no push received within 1s")
	}
}

// The coordinator is authoritative for the asset set: an asset-only
// change lands and reports a change, an asset absent from the push is
// removed, and a set that could never be valid is refused before
// anything is written.
func TestWriteProviderFiles_ReplacesTheAssetSet(t *testing.T) {
	store := newFanOutTestStore(t)
	cfg, err := store.ReadProviderConfigBytes("ollama")
	if err != nil {
		t.Fatalf("ReadProviderConfigBytes: %v", err)
	}
	write := func(set map[string][]byte) (bool, error) {
		return store.WriteProviderFiles("ollama", pkgConfig.KindExternal, pkgConfig.ProviderFiles{Config: cfg, Assets: set})
	}
	dir, err := store.Assets("ollama")
	if err != nil {
		t.Fatalf("Assets: %v", err)
	}

	changed, err := write(map[string][]byte{"a": []byte("1"), "b": []byte("2")})
	if err != nil || !changed {
		t.Fatalf("asset-only change: changed=%v err=%v", changed, err)
	}
	changed, err = write(map[string][]byte{"a": []byte("1")})
	if err != nil || !changed {
		t.Fatalf("removal: changed=%v err=%v", changed, err)
	}
	if got, _ := dir.ReadAll(); len(got) != 1 || string(got["a"]) != "1" {
		t.Errorf("worker set = %v, want only a", got)
	}
	changed, err = write(map[string][]byte{"a": []byte("1")})
	if err != nil || changed {
		t.Errorf("identical set: changed=%v err=%v", changed, err)
	}

	edited := append(append([]byte{}, cfg...), []byte("\n# drift\n")...)
	_, err = store.WriteProviderFiles("ollama", pkgConfig.KindExternal, pkgConfig.ProviderFiles{
		Config: edited, Assets: map[string][]byte{"../escape": []byte("x")},
	})
	if !errors.Is(err, assets.ErrInvalidName) || !errors.Is(err, assets.ErrInvalidSet) {
		t.Fatalf("invalid set: err=%v, want ErrInvalidName wrapped as ErrInvalidSet", err)
	}
	if after, _ := store.ReadProviderConfigBytes("ollama"); !bytes.Equal(after, cfg) {
		t.Error("a refused push must not write the config either")
	}
}

// The body cap is derived from the asset caps; a provider at any of its
// limits (the most bytes, or the most entries with the longest names)
// must still fit, or a legal set could never reach a worker.
func TestProviderSyncMaxBodyFitsTheLargestLegalSet(t *testing.T) {
	mostBytes := map[string][]byte{}
	for i := range assets.MaxProviderBytes / assets.MaxAssetBytes {
		mostBytes[strings.Repeat("n", assets.MaxNameLen-1)+string(rune('a'+i))] = bytes.Repeat([]byte{0xff}, assets.MaxAssetBytes)
	}
	mostEntries := map[string][]byte{}
	share := assets.MaxProviderBytes / assets.MaxAssets
	for i := range assets.MaxAssets {
		name := fmt.Sprintf("%0*d", assets.MaxNameLen, i)
		mostEntries[name] = bytes.Repeat([]byte{0xff}, share)
	}
	for label, set := range map[string]map[string][]byte{"most bytes": mostBytes, "most entries": mostEntries} {
		assertPushFits(t, label, set)
	}
}

// yamlFramingHeadroom leaves room in the YAML budget for the body's own
// JSON framing; it is far smaller than the asset entries' overhead.
const yamlFramingHeadroom = 64

func assertPushFits(t *testing.T, label string, set map[string][]byte) {
	t.Helper()
	if err := assets.ValidateSet(set); err != nil {
		t.Fatalf("%s: the fixture must be a legal set: %v", label, err)
	}
	// YAML that fills its budget less a little framing headroom, so the
	// assets' terms of the cap must each hold on their own.
	yaml := bytes.Repeat([]byte("x"), providerSyncYAMLBudget*3/8-yamlFramingHeadroom)
	body, err := json.Marshal(ProviderSyncBody{
		Kind:       pkgConfig.KindOnDemand,
		ConfigYAML: yaml,
		SchemaYAML: yaml,
		Assets:     set,
	})
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	if len(body) > providerSyncMaxBodyBytes {
		t.Errorf("%s: largest legal push is %d bytes, cap is %d", label, len(body), providerSyncMaxBodyBytes)
	}
}

// A config may name assets, so it must never land without them: when the
// asset set cannot be written, the old config stays on disk and loaded.
func TestWriteProviderFiles_AssetFailureKeepsOldConfig(t *testing.T) {
	store := newFanOutTestStore(t)
	cfg, err := store.ReadProviderConfigBytes("ollama")
	if err != nil {
		t.Fatalf("ReadProviderConfigBytes: %v", err)
	}
	// An assets path that is not a directory makes the write fail.
	if err := os.WriteFile(filepath.Join(store.DirPath(), "external", "ollama", assets.DirName), []byte("x"), 0o600); err != nil {
		t.Fatalf("plant: %v", err)
	}

	edited := append(append([]byte{}, cfg...), []byte("\n# names t.jinja\n")...)
	_, err = store.WriteProviderFiles("ollama", pkgConfig.KindExternal, pkgConfig.ProviderFiles{
		Config: edited, Assets: map[string][]byte{"t.jinja": []byte("x")},
	})
	if err == nil {
		t.Fatal("expected the asset write to fail")
	}
	if after, _ := store.ReadProviderConfigBytes("ollama"); !bytes.Equal(after, cfg) {
		t.Error("the new config landed without the assets it names")
	}
}
