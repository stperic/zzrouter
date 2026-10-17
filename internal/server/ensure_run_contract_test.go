package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/internal/testproc"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/dispatch/chain"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
	"github.com/stperic/zzrouter/pkg/prov_apps"
	"github.com/stperic/zzrouter/pkg/prov_apps/health"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/prov_apps/keepalive"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scenario 4 of the EnsureRun contract (pkg/prov_apps/ensure_run_contract_test.go)
// lives here because readiness waiting does: a readiness waiter holds the run
// it waits on, so a crash reaches it as that run's cause even when a later
// caller has already replaced the run.

const (
	readinessWaiters = 16
	readinessWait    = 10 * time.Second
)

// crashingEngine stays loading until $0.crash exists, then exits; every
// later launch of it becomes ready. It ends with its test process (testproc).
var crashingEngine = testproc.Script(`trap 'kill $! 2>/dev/null; exit 0' TERM INT
if [ -e "$0.crashed" ]; then
  printf 'READY\n'
  while kill -0 $test_owner; do /bin/sleep 1 & wait $!; done
  exit 0
fi
while kill -0 $test_owner && [ ! -e "$0.crash" ]; do /bin/sleep 0.01; done
kill -0 $test_owner || exit 0
touch "$0.crashed"
echo 'engine crashed while loading' >&2
exit 3
`)

type readinessRig struct {
	s      *Server
	mgr    *prov_apps.ProviderAppManager
	engine string
}

func newReadinessRig(t *testing.T, opts ...prov_apps.Option) *readinessRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process fixture")
	}
	r := &readinessRig{engine: filepath.Join(t.TempDir(), "engine")}
	require.NoError(t, os.WriteFile(r.engine, []byte(crashingEngine), 0o700))
	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("fake", config.ServiceConfig{
		Enabled: new(true), Name: "fake", Protocol: config.ProtocolOpenAI, Mode: "on-demand",
		Runtime: &config.AppRuntimeConfig{
			PortRange: []int{8780, 8789}, BasePort: 8780,
			Execution: config.ExecutionConfig{Type: "cli", Command: r.engine},
			HealthCheck: config.HealthcheckConfig{ReadinessProbe: &config.ProbeConfig{
				Timeout: "30s", LogPatterns: config.LogPatternsConfig{Success: []config.PatternConfig{{Pattern: "READY"}}},
			}},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	mgr, err := prov_apps.NewProviderAppManager(cfg, opts...)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		assert.NoError(t, mgr.Stop(ctx))
	})
	r.mgr = mgr
	r.s = &Server{}
	r.s.providers.appMgr = mgr
	return r
}

type readinessOutcome struct {
	run *instance.Instance
	err error
}

// loadingRun starts the crashing engine and parks waiters on its readiness.
func (r *readinessRig) loadingRun(t *testing.T) (*instance.Instance, []chan readinessOutcome) {
	t.Helper()
	result, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "m"})
	require.NoError(t, err)
	outs := make([]chan readinessOutcome, readinessWaiters)
	for i := range outs {
		outs[i] = make(chan readinessOutcome, 1)
		go func() {
			run, err := r.s.loadModelViaNodeAPI("m", "fake", "chat")
			outs[i] <- readinessOutcome{run, err}
		}()
	}
	waitParkedIn(t, ").loadModelViaNodeAPI(", readinessWaiters)
	return result.Run, outs
}

func (r *readinessRig) crash(t *testing.T, run *instance.Instance) {
	t.Helper()
	require.NoError(t, os.WriteFile(r.engine+".crash", nil, 0o600))
	select {
	case <-run.LeftStarting():
	case <-time.After(readinessWait):
		t.Fatal("run never left starting")
	}
	require.Equal(t, instance.StatusFailed, run.GetStatus())
}

// assertCause requires every waiter to report the crashed run's own failure.
func assertCause(t *testing.T, run *instance.Instance, outs []chan readinessOutcome) {
	t.Helper()
	for _, out := range outs {
		var o readinessOutcome
		select {
		case o = <-out:
		case <-time.After(readinessWait):
			t.Fatal("a readiness waiter never returned")
		}
		require.Error(t, o.err)
		assert.Nil(t, o.run)
		if cause := run.FailureCause(); cause != nil {
			assert.ErrorIs(t, o.err, cause)
		} else {
			assert.ErrorContains(t, o.err, run.GetErrorMessage())
		}
	}
}

func TestEnsureRunContractReadinessWaiters(t *testing.T) {
	t.Run("4a crash while loading reaches every waiter", func(t *testing.T) {
		r := newReadinessRig(t)
		run, outs := r.loadingRun(t)
		r.crash(t, run)
		assertCause(t, run, outs)
	})

	t.Run("4b a replacement never hides the cause from an old waiter", func(t *testing.T) {
		r := newReadinessRig(t)
		replacedOld := make(chan struct{})
		r.s.providers.readinessWoken = func() { <-replacedOld } // waiters look only once the old run is stopped
		run, outs := r.loadingRun(t)
		r.crash(t, run)
		replaced, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "fake", Model: "m"})
		require.NoError(t, err)
		require.NotEqual(t, run.ID, replaced.Run.ID)
		require.NotEqual(t, instance.StatusFailed, run.GetStatus(), "replacing the failed run stopped it")
		close(replacedOld)
		assertCause(t, run, outs)
		select {
		case <-replaced.Run.LeftStarting():
		case <-time.After(readinessWait):
			t.Fatal("replacement never left starting")
		}
	})
}

// waitParkedIn returns once exactly n goroutines through frame exist and all
// are blocked, on two consecutive looks.
func waitParkedIn(t *testing.T, frame string, n int) {
	t.Helper()
	blockedStates := []string{"chan receive", "chan send", "select", "semacquire", "sync.Cond.Wait", "sync.Mutex.Lock", "sync.RWMutex.Lock", "sync.RWMutex.RLock", "sync.WaitGroup.Wait"}
	stable, total, blocked := 0, 0, 0
	for end := time.Now().Add(readinessWait); time.Now().Before(end); runtime.Gosched() {
		buf := make([]byte, 1<<20)
		size := runtime.Stack(buf, true)
		for size == len(buf) {
			buf = make([]byte, 2*len(buf))
			size = runtime.Stack(buf, true)
		}
		buf = buf[:size]
		total, blocked = 0, 0
		for _, g := range strings.Split(string(buf), "\n\n") {
			if !strings.Contains(g, frame) {
				continue
			}
			total++
			header, _, _ := strings.Cut(g, "\n")
			_, state, _ := strings.Cut(header, "[")
			for _, s := range blockedStates {
				if strings.HasPrefix(state, s) {
					blocked++
					break
				}
			}
		}
		if total == n && blocked == n {
			if stable++; stable == 2 {
				return
			}
			continue
		}
		stable = 0
		time.Sleep(time.Millisecond) // poll interval, not synchronization
	}
	t.Fatalf("want %d goroutines parked in %s, have %d (%d blocked)", n, frame, total, blocked)
}

// The served inference contract: a request to an Ollama model reaches its
// run through the run's own provider target and holds a lease for it, on
// every path that posts to a run.

const (
	servedDaemonHost  = "remote-daemon:11434"
	servedCredential  = "daemon-credential-0123456789abcdef" //nolint:gosec // a fake daemon's token, sent to a fake transport
	servedCallerKey   = "caller-key-0123456789abcdef0123456789"
	servedClientModel = "m"
	servedDaemonModel = "m:latest"
	servedSettle      = 50 * time.Millisecond // a window in which nothing must happen
)

// inferenceSeen is one inference request the daemon answered.
type inferenceSeen struct {
	host, path, auth string
	body             map[string]any
	active           int64
}

// servedDaemon is an Ollama daemon behind the injected client: it loads,
// unloads and lists models, and answers inference while recording what it
// was sent and how many leases the run held at that moment.
type servedDaemon struct {
	mu      sync.Mutex
	loaded  map[string]bool
	unloads int
	seen    []inferenceSeen
	run     func() *instance.Instance
}

func (d *servedDaemon) RoundTrip(req *http.Request) (*http.Response, error) {
	reply := func(body string) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
	}
	var body map[string]any
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&body)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch req.URL.Path {
	case "/v1/models":
		return reply(`{"data":[]}`), nil
	case "/api/ps":
		var models []string
		for name := range d.loaded {
			models = append(models, `{"name":"`+name+`","model":"`+name+`","expires_at":"`+time.Now().Add(time.Hour).Format(time.RFC3339)+`"}`)
		}
		return reply(`{"models":[` + strings.Join(models, ",") + `]}`), nil
	}
	model, _ := body["model"].(string)
	_, prompted := body["prompt"]
	_, chatted := body["messages"]
	if keepAlive, named := body["keep_alive"]; named && keepAlive == float64(0) && !prompted && !chatted {
		d.unloads++
		delete(d.loaded, model)
		return reply(`{"done":true}`), nil
	}
	if input, isLoad := body["input"].([]any); req.URL.Path == "/api/embed" && isLoad && len(input) == 0 {
		d.loaded[model] = true
		return reply(`{"done":true}`), nil
	}
	seen := inferenceSeen{host: req.URL.Host, path: req.URL.Path, auth: req.Header.Get("Authorization"), body: body}
	if d.run != nil {
		if run := d.run(); run != nil {
			seen.active = run.GetActiveRequests()
		}
	}
	d.seen = append(d.seen, seen)
	return reply(`{"model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"message":{"role":"assistant","content":"hi"},"done":true}`), nil
}

func (d *servedDaemon) last(t *testing.T) inferenceSeen {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	require.NotEmpty(t, d.seen, "the daemon answered no inference")
	return d.seen[len(d.seen)-1]
}

func (d *servedDaemon) unloaded() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unloads
}

type servedServerRig struct {
	s      *Server
	mgr    *prov_apps.ProviderAppManager
	cfg    *config.AppsConfig
	daemon *servedDaemon
}

// newServedServerRig serves ollama from a token-protected daemon on another host.
func newServedServerRig(t *testing.T) *servedServerRig {
	t.Helper()
	r := &servedServerRig{cfg: &config.AppsConfig{}, daemon: &servedDaemon{loaded: map[string]bool{}}}
	require.NoError(t, r.cfg.AddApp("ollama", config.ServiceConfig{
		Enabled: new(true), Name: "ollama", Protocol: config.ProtocolOllama, Mode: "external",
		Runtime: &config.AppRuntimeConfig{
			Endpoint: "http://" + servedDaemonHost, KeepAlive: "5m",
			API: &config.APIConfig{AuthType: config.AuthTypeBearer, Token: servedCredential},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	client := &http.Client{Transport: r.daemon}
	resolver := backend.NewResolver(func() *config.AppsConfig { return r.cfg })
	mgr, err := prov_apps.NewProviderAppManager(r.cfg, prov_apps.WithHTTPClient(client), prov_apps.WithBackendResolver(resolver.Resolve))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), readinessWait)
		defer cancel()
		assert.NoError(t, mgr.Stop(ctx))
	})
	nc := &config.NodeConfig{}
	nc.Node.Name = "node-a"
	s := &Server{node: NewNodeIdentity(nc), config: nc, configStore: config.NewMemoryAppsConfigStore(r.cfg), backend: resolver, httpStreamingClient: client, model: &model.Subsystem{}}
	s.inference.normalizers = NewResponseNormalizers()
	s.proxy = NewProxyClient(client, nil, s.inference.normalizers, nil, nil, nil, nil)
	s.providers.appMgr = mgr
	r.s, r.mgr = s, mgr
	return r
}

// served starts a run for the client's name and waits for it to be ready.
func (r *servedServerRig) served(t *testing.T) *instance.Instance {
	t.Helper()
	result, err := r.mgr.EnsureRun(t.Context(), prov_apps.EnsureRequest{Provider: "ollama", Model: servedClientModel, Origin: prov_apps.OriginRequest})
	require.NoError(t, err)
	require.NotNil(t, result.Run, "a served model is a run")
	select {
	case <-result.Run.LeftStarting():
	case <-time.After(readinessWait):
		t.Fatal("run never left starting")
	}
	require.Equal(t, instance.StatusRunning, result.Run.GetStatus(), result.Run.GetErrorMessage())
	r.daemon.run = func() *instance.Instance { return result.Run }
	return result.Run
}

// inference is a request as the handler edge leaves it: body, model and hints in the context.
func inference(path string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+servedCallerKey)
	ctx := context.WithValue(req.Context(), CtxKeyModel, servedClientModel)
	ctx = context.WithValue(ctx, CtxKeyOriginalBody, body)
	ctx = context.WithValue(ctx, CtxKeyRequestHints, keepalive.Parse(body))
	return req.WithContext(ctx)
}

func TestEnsureRunContractServedInference(t *testing.T) {
	t.Run("S10 a served run is reached at its provider, with its credential, under a lease", func(t *testing.T) {
		r := newServedServerRig(t)
		run := r.served(t)
		native := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false,"keep_alive":"10m"}`)
		w := httptest.NewRecorder()
		r.s.forwardToRun(w, inference("/api/chat", native), run)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		seen := r.daemon.last(t)
		assert.Equal(t, servedDaemonHost, seen.host)
		assert.Equal(t, "Bearer "+servedCredential, seen.auth, "the provider's credential, never the caller's key")
		assert.Equal(t, "10m", seen.body["keep_alive"], "the client's keep_alive passes through")
		assert.Equal(t, servedClientModel, seen.body["model"], "the daemon resolves an untagged name itself")
		assert.Equal(t, int64(1), seen.active, "the request holds a lease")
		assert.Zero(t, run.GetActiveRequests())
		assert.Equal(t, 10*time.Minute, run.GetKeepAlive(), "the client's keep_alive is the run's hint")

		openAI := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		w = httptest.NewRecorder()
		r.s.proxyToInstance(w, inference("/v1/chat/completions", openAI), run, true)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		seen = r.daemon.last(t)
		assert.Equal(t, servedDaemonHost, seen.host)
		assert.Equal(t, "Bearer "+servedCredential, seen.auth)
		assert.NotContains(t, seen.body, "keep_alive", "a body that names no keep_alive gets none")
		assert.Equal(t, servedClientModel, seen.body["model"])
		assert.Equal(t, int64(1), seen.active)
		assert.Zero(t, run.GetActiveRequests())
	})

	t.Run("S15 a client keep_alive 0 unloads once, after the response", func(t *testing.T) {
		r := newServedServerRig(t)
		r.mgr.Start(t.Context())
		run := r.served(t)
		w := httptest.NewRecorder()
		r.s.forwardToRun(w, inference("/api/generate", []byte(`{"model":"m","prompt":"hi","stream":false,"keep_alive":0}`)), run)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, float64(0), r.daemon.last(t).body["keep_alive"], "the client's keep_alive passes through")
		require.Eventually(t, func() bool { return len(r.mgr.Instances().List()) == 0 }, readinessWait, 10*time.Millisecond)
		time.Sleep(servedSettle)
		assert.Equal(t, 1, r.daemon.unloaded())
	})

	t.Run("S22 a group replica on a served provider leases its run", func(t *testing.T) {
		r := newServedServerRig(t)
		strategies := fallback.NewStrategyRegistry()
		strategies.Register(&fallback.PriorityStrategy{})
		proxy := chain.New(newChainServerDeps(r.s), fallback.NewCooldownManager(), fallback.NewCooldownManager(), strategies, fallback.NewLoadTracker(), nil, nil)
		body := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}]}`)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = inference("/v1/chat/completions", body)
		httperr.AttachResponder(openaiproto.New())(c)
		r.daemon.run = func() *instance.Instance {
			run, _ := r.mgr.Instances().GetByKey(instance.NewKey("", servedDaemonModel, ""))
			return run
		}
		resolved := &resolver.Resolved{OriginalName: "g", GroupName: "g", Strategy: "priority", Candidates: []fallback.Candidate{{Name: "r1", App: "ollama", Model: servedClientModel}}}
		proxy.ProxyWithFallback(c, resolved, body, llm.NewInferenceRecorder(t.Context(), "g", ""), "g", "", false)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		seen := r.daemon.last(t)
		assert.Equal(t, servedDaemonHost, seen.host)
		assert.Equal(t, "Bearer "+servedCredential, seen.auth)
		assert.Equal(t, int64(1), seen.active, "the replica's request holds a lease")
		run, ok := r.mgr.Instances().GetByKey(instance.NewKey("", servedDaemonModel, ""))
		require.True(t, ok, "the replica's model became a run")
		assert.Equal(t, instance.StartedByRequest, run.StartedBy)
		assert.False(t, run.GetLastActivity().IsZero(), "the lease stamped activity")
		assert.Zero(t, run.GetActiveRequests())
	})
}

// Every path that posts to a run opens it the same way: a refused lease is
// answered, and a client already gone is not.
func TestForwardToRunAnswersAsEveryRunPath(t *testing.T) {
	native := []byte(`{"model":"m","prompt":"hi","stream":false}`)
	t.Run("refused lease", func(t *testing.T) {
		r := newServedServerRig(t)
		run := r.served(t)
		run.SetStatus(instance.StatusStopping)
		w := httptest.NewRecorder()
		r.s.forwardToRun(w, inference("/api/generate", native), run)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.Empty(t, r.daemon.seen)
	})
	t.Run("client gone", func(t *testing.T) {
		r := newServedServerRig(t)
		run := r.served(t)
		req := inference("/api/generate", native)
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		w := httptest.NewRecorder()
		r.s.forwardToRun(w, req.WithContext(ctx), run)
		assert.Empty(t, w.Body.String())
		assert.Empty(t, r.daemon.seen)
		assert.Zero(t, run.GetActiveRequests())
	})
}

func TestServedSurfaces(t *testing.T) {
	t.Run("an unload of a model with no run goes to the daemon and loads nothing", func(t *testing.T) {
		r := newServedServerRig(t)
		w := httptest.NewRecorder()
		require.NoError(t, r.s.serveLocalOllama(w, inference("/api/generate", []byte(`{"model":"m","keep_alive":0}`)), servedClientModel))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, 1, r.daemon.unloaded(), "the daemon answers the unload itself")
		assert.Empty(t, r.daemon.loaded, "nothing was loaded first")
		assert.Empty(t, r.mgr.Instances().List())
	})
	t.Run("/api/ps renders what the daemon reported", func(t *testing.T) {
		r := newServedServerRig(t)
		run := r.served(t)
		expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
		run.SetLoaded(instance.Loaded{Size: 2, VRAM: 1, ContextLength: 4096, ExpiresAt: expires, Digest: "sha-m", Format: "gguf", Family: "llama", ParameterSize: "8B", QuantLevel: "Q4_K_M"})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/zzrouter/v1/internal/ollama/ps", nil)
		(&OllamaHandlers{appMgr: r.mgr, nodeName: func() string { return "node-a" }}).HandleOllamaPsLocal(c)
		var ps OllamaPsResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ps))
		require.Len(t, ps.Models, 1)
		assert.Equal(t, OllamaPsEntry{Name: servedDaemonModel + "@node-a", Model: servedDaemonModel, Size: 2, SizeVRAM: 1, ContextLength: 4096, ExpiresAt: expires, Digest: "sha-m",
			Details: OllamaModelDetails{Format: "gguf", Family: "llama", ParameterSize: "8B", QuantLevel: "Q4_K_M"}}, ps.Models[0])
	})
	t.Run("a served run's log is its lifecycle, readable where the 404 points once the daemon drops it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newServedServerRig(t)
			r.mgr.Start(t.Context()) // the services ticker reconciles
			executor := &RunsExecutor{appMgr: r.mgr, getNodename: r.s.node.Nodename}
			internal := gin.New()
			internal.GET("/zzrouter/v1/internal/runs", executor.HandleInternalListRuns)
			internal.GET("/zzrouter/v1/internal/runs/:id", executor.HandleInternalGetRun)
			ctrl := NewRunsController(NewRunsService(&lifecycleRouter{engine: internal}), nil, nil, nil, nil)
			logs := &LogsHandlers{appMgr: r.mgr, getClusterClient: func() mesh.ClusterClient { return nil }, isLocalNode: func(n string) bool { return n == "node-a" }}
			public := gin.New()
			public.GET("/zzrouter/v1/runs/:id", ctrl.GetRun)
			public.GET("/zzrouter/v1/runs/:id/logs", logs.HandleGetRunLogsPublic)
			get := func(path string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				public.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				return rec
			}

			run := r.served(t)
			rec := get("/zzrouter/v1/runs/" + run.ID + "/logs")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Regexp(t, `load requested, keep-alive 5m0s.*loaded`, rec.Body.String())

			r.daemon.mu.Lock()
			delete(r.daemon.loaded, servedDaemonModel)
			r.daemon.mu.Unlock()
			require.Eventually(t, func() bool { _, live := r.mgr.GetInstance(run.ID); return !live }, 3*health.DefaultInterval, servedSettle)
			rec = get("/zzrouter/v1/runs/" + run.ID)
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			var problem struct {
				Detail string `json:"detail"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			_, advice, found := strings.Cut(problem.Detail, "its log stays readable at GET ")
			require.True(t, found, problem.Detail)
			rec = get(strings.Replace(advice, "<node it ran on>", "node-a", 1))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "dropped by the daemon")
			require.NoError(t, r.mgr.Stop(context.Background()))
			// Stop leaves the trailing audit timer active; advance only virtual time.
			synctest.Wait()
			time.Sleep(health.DefaultInterval)
			synctest.Wait()
		})
	})
	t.Run("a batch launch with no model is refused and loads nothing", func(t *testing.T) {
		r := newServedServerRig(t)
		launched, failed, err := NewRunsUtilityService(r.mgr, func() *config.AppsConfig { return r.cfg }).BatchLaunch(t.Context(), []LaunchRequest{{Provider: "ollama", LaunchMode: "native"}})
		require.NoError(t, err)
		assert.Empty(t, launched)
		require.Len(t, failed, 1)
		assert.Contains(t, failed[0]["error"], "names its model")
		assert.Empty(t, r.daemon.loaded)
		assert.Empty(t, r.mgr.Instances().List())
	})
}
