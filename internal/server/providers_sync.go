package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/assets"
	"github.com/stperic/zzrouter/pkg/prov_apps/schema"
)

// ProviderSyncBody is the wire shape of /internal/sync/providers/:name.
// Workers receive full bytes of config.yaml (and optionally schema.yaml)
// so the receiver doesn't have to diff against its current tree —
// last-write-wins keyed on coordinator state.
type ProviderSyncBody struct {
	Kind       pkgConfig.Kind `json:"kind"`
	ConfigYAML []byte         `json:"config_yaml"`
	// SchemaYAML is nil when the coordinator has no schema.yaml for this
	// provider; []byte{} would mean "delete the sibling" but the current
	// coordinator never sends that (schema.yaml is shipped from templates
	// and not mutated).
	SchemaYAML []byte `json:"schema_yaml,omitempty"`
	// Assets is the provider's whole asset set; the worker removes any
	// asset not in it, so null or absent means the provider has none.
	Assets map[string][]byte `json:"assets"`
}

// HandleProviderSync writes the incoming config + schema bytes to the
// local providers/ tree and triggers a reload. Coordinator-only caller
// is enforced by the mTLS middleware on /internal/*; this handler
// additionally refuses on non-worker modes so a standalone or
// coordinator can't be tricked into overwriting itself via a spoofed
// peer. Return 200 on success, 4xx on validation, 5xx on write failure.
func (s *Server) HandleProviderSync(c *gin.Context) {
	if s.node == nil || !s.node.IsWorker() {
		Forbidden(c, "provider sync only accepted on worker nodes")
		return
	}
	if s.configStore == nil {
		ServiceUnavailable(c, "providers config store not ready")
		return
	}
	name := c.Param("name")
	if name == "" {
		BadRequest(c, "provider name required")
		return
	}
	// M1 — cap body size so a misbehaving peer can't OOM a worker even
	// under mTLS-trusted senders.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, providerSyncMaxBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		BadRequest(c, "read body: "+err.Error())
		return
	}
	var body ProviderSyncBody
	if err := json.Unmarshal(raw, &body); err != nil {
		BadRequest(c, "decode body: "+err.Error())
		return
	}
	// Re-validate inbound bytes against the same JSON-Schema + per-provider
	// Validate() pipeline used at on-disk load time. The post-write reload
	// would also catch a malformed config, but only after the bytes had
	// already landed and the receiver's in-memory state went stale; failing
	// closed at the wire is cleaner and gives the coord a fast 4xx for
	// future retry/alerting.
	if p, err := pkgConfig.DecodeProviderBytes(body.Kind, name, body.ConfigYAML); err != nil {
		BadRequest(c, "validate config: "+err.Error())
		return
	} else if err := p.Validate(); err != nil {
		BadRequest(c, "validate config: "+err.Error())
		return
	}
	if len(body.SchemaYAML) != 0 {
		declared, err := schema.LoadYAMLSchema(body.SchemaYAML)
		if err != nil {
			BadRequest(c, "validate schema: "+err.Error())
			return
		}
		_, errs := schema.Merge(name, declared)
		if len(errs) != 0 {
			BadRequest(c, "validate schema: "+errors.Join(errs...).Error())
			return
		}
	}
	changed, err := s.configStore.WriteProviderFiles(name, body.Kind, pkgConfig.ProviderFiles{
		Config: body.ConfigYAML,
		Schema: body.SchemaYAML,
		Assets: body.Assets,
	})
	if errors.Is(err, assets.ErrInvalidSet) {
		BadRequest(c, "validate assets: "+err.Error())
		return
	}
	if err != nil {
		InternalNodeError(c, err.Error())
		return
	}
	// `changed` is what makes a scheduled push cheap: the coordinator
	// re-sends the whole tree on a timer to repair drift, and a worker
	// already in step writes nothing and reloads nothing.
	c.JSON(http.StatusOK, gin.H{"name": name, "bytes": len(body.ConfigYAML), "changed": changed})
}

// reconcileProviderTree pushes the provider tree to every worker,
// whether or not anything changed. It is the repair path for the three
// ways a worker ends up out of step that no mutation event covers:
//
//   - a config.yaml edited on disk while the coordinator was stopped:
//     the store loads it at boot, which is not a change, so no listener
//     ever fires and the workers keep the old bytes indefinitely;
//   - a worker that was unreachable when the event did fire: the push
//     failed, was logged, and nothing retried it;
//   - a file edited on the worker itself, which the MANAGED FILE banner
//     in every config.yaml says the coordinator owns.
//
// Safe to call on a timer because the receiving end compares bytes
// before writing: a worker already in step reloads nothing.
func (s *Server) reconcileProviderTree(reason string) {
	if s == nil || s.providers.syncFanOut == nil || s.configStore == nil {
		return
	}
	cfg := s.configStore.Config()
	if cfg == nil {
		return
	}
	if d := s.providers.syncFanOut.fire(cfg); d == pkgConfig.DispositionApplied {
		slog.Debug("[ProvSync] reconcile fired", "reason", reason)
	}
}

// awaitProviderSync blocks until every pending provider-tree push has
// been delivered, and reports where it got to. A node with no fan-out
// (standalone, worker) has nobody to wait for and is always in step
// with itself.
func (s *Server) awaitProviderSync(ctx context.Context) SyncReport {
	if s == nil || s.providers.syncFanOut == nil {
		return SyncReport{State: SyncComplete}
	}
	return s.providers.syncFanOut.AwaitQuiesce(ctx)
}

// providerSyncFanOut runs on the coordinator after every AppsConfigStore
// mutation: for each provider, push its config.yaml (+ schema.yaml if
// present) to every known worker endpoint. A nil dependency (no
// listener, no endpoints, not coordinator) is silently a no-op so
// standalone + pre-pair boot paths stay clean.
type providerSyncFanOut struct {
	store         *pkgConfig.AppsConfigStore
	isCoord       func() bool
	listEndpoints func() []*mesh.Endpoint
	dialClient    func() *http.Client

	// Single-flight guard: at most one fan-out in flight per invocation
	// burst. Rapid successive PATCHes coalesce into one push round after
	// the current one completes.
	mu         sync.Mutex
	inFlight   bool
	repeatPend bool
	// idle is closed while no round is running, and replaced with an
	// open channel for the duration of one. AwaitQuiesce reads it, so a
	// caller can wait for its own write to reach the workers instead of
	// answering on a push that has not happened yet.
	idle chan struct{}
	// last is what the last completed round reached. A round that
	// quiesced is not a round that reached everybody: a worker that is
	// down is exactly the case where a caller must not be told its write
	// is in effect cluster-wide.
	last pushRound
	// writes counts the store mutations seen, and current is the round in
	// flight. A waiter that gives up before the round ends can still be
	// told which workers it has reached, if it carries the waiter's write:
	// a worker asleep on connect must not hide one that took the push.
	writes  uint64
	current *roundProgress
}

// SyncState is what a write can say about where its config has got to.
type SyncState string

const (
	// SyncComplete — every known worker took the tree.
	SyncComplete SyncState = "complete"
	// SyncPartial — the round finished with at least one worker left
	// on the previous tree.
	SyncPartial SyncState = "partial"
	// SyncPending — the push was still running when the caller stopped
	// waiting.
	SyncPending SyncState = "pending"
)

// SyncReport is where a write's provider tree got to: the state, and the
// workers known to have it. On SyncPending none is known to.
type SyncReport struct {
	State SyncState
	// Landed names each worker that took every push of the round. A
	// worker not yet known by name is never listed, so it is never
	// vouched for.
	Landed []string
}

// Reached reports whether worker node is known to have the tree. The
// coordinator itself is not a worker: it holds every write it makes.
func (r SyncReport) Reached(node string) bool {
	return slices.Contains(r.Landed, node)
}

// providerSyncWaitTimeout bounds how long a provider-tree write waits for
// the workers to have it. Long enough for an mTLS dial plus one push
// round to every worker on a LAN; past it the write still stands and
// the reconcile on the maintenance tick repairs whoever missed it.
const providerSyncWaitTimeout = 10 * time.Second

// headerProviderSync tells the caller how far the tree it just wrote
// has got: `complete` when every known worker took it — including the
// trivial case of having none — `partial` when the round finished with
// one left behind, `pending` when the push outlived the wait. Only
// `complete` means the next launch anywhere resolves this value.
const headerProviderSync = "X-Provider-Sync"

// providerSyncWait waits for the push a provider-tree write started.
type providerSyncWait func(context.Context) SyncReport

// await waits, bounded, for that push, stamps the answer on the response
// and returns it. A nil wait is a node no other node reads from, so the
// write is already everywhere it needs to be.
func (w providerSyncWait) await(c *gin.Context) SyncReport {
	if w == nil {
		return SyncReport{State: SyncComplete}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), providerSyncWaitTimeout)
	defer cancel()
	report := w(ctx)
	if report.State != SyncComplete {
		slog.Warn("provider sync did not reach every worker",
			"state", report.State, "landed", report.Landed, "timeout", providerSyncWaitTimeout)
	}
	c.Header(headerProviderSync, string(report.State))
	return report
}

// newProviderSyncFanOut wires a fan-out handle. Dependencies are lazy
// funcs so role transitions (standalone → coord) resolve correctly.
func newProviderSyncFanOut(
	store *pkgConfig.AppsConfigStore,
	isCoord func() bool,
	listEndpoints func() []*mesh.Endpoint,
	dialClient func() *http.Client,
) *providerSyncFanOut {
	closed := make(chan struct{})
	close(closed)
	return &providerSyncFanOut{
		// No round has run, so none has left a worker behind.
		last:          pushRound{complete: true},
		store:         store,
		isCoord:       isCoord,
		listEndpoints: listEndpoints,
		dialClient:    dialClient,
		idle:          closed,
	}
}

// Listener returns the OnChange callback. Reports
// DispositionApplied when at least one worker accepted the push,
// DispositionIgnored on standalone / no workers, and DispositionRejected
// when every attempted push failed.
func (f *providerSyncFanOut) Listener() func(*pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
	return func(cfg *pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
		return f.fire(cfg)
	}
}

// fire is the entry point both the listener and the re-fire path take.
// Extracted so single-flight state transitions live in one readable
// place — the deferred cleanup in run() can invoke fire without risk
// of re-entering Listener's closure.
func (f *providerSyncFanOut) fire(cfg *pkgConfig.AppsConfig) pkgConfig.ReloadDisposition {
	if f == nil || f.store == nil || f.isCoord == nil || !f.isCoord() {
		return pkgConfig.DispositionIgnored
	}
	if cfg == nil {
		return pkgConfig.DispositionIgnored
	}
	f.mu.Lock()
	f.writes++
	if f.inFlight {
		f.repeatPend = true
		f.mu.Unlock()
		return pkgConfig.DispositionIgnored
	}
	f.inFlight = true
	f.idle = make(chan struct{})
	f.current = &roundProgress{carries: f.writes}
	f.mu.Unlock()
	go f.run(cfg)
	return pkgConfig.DispositionApplied
}

// AwaitQuiesce blocks until no push round is running or pending, and
// reports where the tree got to, so a caller that just mutated the
// store answers on a config the workers actually have. The write stands
// whatever this returns.
//
// Listeners run inside the store mutation, so by the time a mutating
// caller reaches here its own round has already been started or folded
// into the one in flight.
func (f *providerSyncFanOut) AwaitQuiesce(ctx context.Context) SyncReport {
	if f == nil {
		return SyncReport{State: SyncComplete}
	}
	f.mu.Lock()
	idle, want := f.idle, f.writes
	f.mu.Unlock()
	select {
	case <-idle:
		f.mu.Lock()
		defer f.mu.Unlock()
		state := SyncPartial
		if f.last.complete {
			state = SyncComplete
		}
		return SyncReport{State: state, Landed: slices.Clone(f.last.landed)}
	case <-ctx.Done():
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.current == nil || f.current.carries < want {
			return SyncReport{State: SyncPending}
		}
		return SyncReport{State: SyncPending, Landed: f.current.landed()}
	}
}

// run pushes the tree, then pushes it again for any mutation that
// arrived while the last push was in flight, until nothing is owed.
func (f *providerSyncFanOut) run(cfg *pkgConfig.AppsConfig) {
	for {
		f.mu.Lock()
		progress := f.current
		f.mu.Unlock()
		round := f.push(cfg, progress)

		f.mu.Lock()
		f.last = round
		if !f.repeatPend {
			// idle closes last, so a waiter never reads quiescence
			// while another round is still owed.
			f.inFlight = false
			close(f.idle)
			f.mu.Unlock()
			return
		}
		f.repeatPend = false
		f.current = &roundProgress{carries: f.writes}
		f.mu.Unlock()

		if latest := f.store.Config(); latest != nil {
			cfg = latest
		}
	}
}

// pushRound is what one fan-out round reached.
type pushRound struct {
	// landed names, sorted, each worker that took every push.
	landed []string
	// complete is whether every target took every push, named or not.
	complete bool
}

// roundProgress tallies one round as its pushes finish.
type roundProgress struct {
	// carries is the count of store writes the round includes.
	carries uint64

	mu      sync.Mutex
	targets []syncTarget
	owed    map[string]int  // by target URL: pushes not yet taken
	missed  map[string]bool // by target URL
	// failedAll is a failure no worker escapes, such as an unreadable
	// provider config.
	failedAll bool
}

// begin records the targets and how many pushes each is owed.
func (p *roundProgress) begin(targets []syncTarget, pushes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = targets
	p.owed = make(map[string]int, len(targets))
	p.missed = map[string]bool{}
	for _, t := range targets {
		p.owed[t.url] = pushes
	}
}

func (p *roundProgress) took(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owed[url]--
}

func (p *roundProgress) miss(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.missed[url] = true
}

func (p *roundProgress) failAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failedAll = true
}

// landed names, sorted, each named worker that has taken every push.
func (p *roundProgress) landed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failedAll {
		return nil
	}
	var names []string
	for _, t := range p.targets {
		if t.node != "" && p.owed[t.url] == 0 && !p.missed[t.url] {
			names = append(names, t.node)
		}
	}
	slices.Sort(names)
	return names
}

// result is the finished round.
func (p *roundProgress) result() pushRound {
	p.mu.Lock()
	complete := !p.failedAll && len(p.missed) == 0
	p.mu.Unlock()
	return pushRound{landed: p.landed(), complete: complete}
}

// push performs one fan-out round: every syncable provider to every
// known worker, tallied in p as it goes. A worker some push missed is
// still resolving launches from the previous tree.
func (f *providerSyncFanOut) push(cfg *pkgConfig.AppsConfig, p *roundProgress) pushRound {
	var targets []syncTarget
	for _, ep := range f.listEndpoints() {
		if t, ok := syncTargetOf(ep); ok {
			targets = append(targets, t)
		}
	}
	// Cloud + registry providers run only on the coordinator (their
	// runtime is HTTP-to-a-remote-API, never a worker-side process).
	// Shipping their configs — which carry cloud API keys in
	// defaults.environment — to workers would leak credentials into
	// worker caches for no operational benefit. Only on-demand and
	// external kinds launch on workers.
	var names []string
	for _, name := range cfg.AppNames() {
		if kind := f.store.ProviderKind(name); kind == pkgConfig.KindOnDemand || kind == pkgConfig.KindExternal {
			names = append(names, name)
		}
	}
	p.begin(targets, len(names))
	if len(targets) == 0 {
		return p.result()
	}
	client := f.dialClient()
	if client == nil {
		slog.Debug("[ProvSync] no mTLS client available; skipping fan-out")
		p.failAll()
		return p.result()
	}

	// M5 — push N providers × M workers in parallel, bounded to
	// providerSyncConcurrency. Serial loop made sustained-PATCH latency
	// linear in N*M.
	sem := make(chan struct{}, providerSyncConcurrency)
	var wg sync.WaitGroup
	for _, name := range names {
		kind := f.store.ProviderKind(name)
		cfgYAML, err := f.store.ReadProviderConfigBytes(name)
		if err != nil {
			slog.Warn("[ProvSync] read config bytes failed", "provider", name, "error", err)
			p.failAll()
			continue
		}
		schemaYAML, _ := f.store.ReadProviderSchemaBytes(name)
		providerAssets, err := f.providerAssets(name)
		if err != nil {
			slog.Warn("[ProvSync] read assets failed", "provider", name, "error", err)
			p.failAll()
			continue
		}
		body, err := json.Marshal(ProviderSyncBody{
			Kind:       kind,
			ConfigYAML: cfgYAML,
			SchemaYAML: schemaYAML,
			Assets:     providerAssets,
		})
		if err != nil {
			slog.Warn("[ProvSync] marshal body failed", "provider", name, "error", err)
			p.failAll()
			continue
		}
		for _, t := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(t syncTarget, name string, body []byte) {
				defer wg.Done()
				defer func() { <-sem }()
				if f.pushOne(client, t.url+"/zzrouter/v1/internal/sync/providers/"+name, name, t.node, body) {
					p.took(t.url)
				} else {
					p.miss(t.url)
				}
			}(t, name, body)
		}
	}
	wg.Wait()
	return p.result()
}

// syncTarget is one worker a round pushes to.
type syncTarget struct {
	url string
	// node is the worker's name; empty while it is not known by one.
	node string
}

// syncTargetOf is the push target for ep; the coordinator itself and an
// endpoint with no address are not targets.
func syncTargetOf(ep *mesh.Endpoint) (syncTarget, bool) {
	if ep == nil || ep.IsLocal {
		return syncTarget{}, false
	}
	url := ep.ClusterURL
	if url == "" {
		url = ep.URL
	}
	if url == "" {
		return syncTarget{}, false
	}
	return syncTarget{url: url, node: ep.NodeName}, true
}

// providerAssets reads the provider's whole asset set for a push.
func (f *providerSyncFanOut) providerAssets(name string) (map[string][]byte, error) {
	dir, err := f.store.Assets(name)
	if err != nil {
		return nil, err
	}
	return dir.ReadAll()
}

// pushOne sends one JSON body to one worker with a short timeout, and
// reports whether that worker took it. Logs failure and moves on — the
// next OnChange re-tries every worker.
func (f *providerSyncFanOut) pushOne(client *http.Client, url, provider, nodeName string, body []byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), providerSyncPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("[ProvSync] build request failed", "provider", provider, "node", nodeName, "error", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("[ProvSync] push failed", "provider", provider, "node", nodeName, "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		slog.Warn("[ProvSync] push rejected", "provider", provider, "node", nodeName, "status", resp.StatusCode)
		return false
	}
	// A push that changed something means the worker had drifted, which
	// is worth a line: on the mutation path it is expected, on the
	// reconcile path it is a repair nobody asked for and would
	// otherwise never learn about.
	var ack struct {
		Changed bool `json:"changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err == nil && ack.Changed {
		slog.Info("[ProvSync] worker config updated", "provider", provider, "node", nodeName)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return true
}

// providerSyncPushTimeout bounds a single worker push. Fan-out is
// best-effort; slow/down workers must not stall the listener chain.
const providerSyncPushTimeout = 10 * time.Second

// providerSyncConcurrency bounds the per-run fan-out goroutine count so
// a large cluster × many-providers sweep doesn't spike mTLS connection
// counts or starve the worker-side accept queue.
const providerSyncConcurrency = 8

// providerSyncMaxBodyBytes caps each POST body: a provider's YAML, which
// is typically <10 KiB, plus its whole asset set at the caps: the bytes
// base64-encoded, as JSON carries []byte, and one "name":"" entry each.
const providerSyncMaxBodyBytes = providerSyncYAMLBudget +
	(assets.MaxProviderBytes+2)/3*4 +
	assets.MaxAssets*(assets.MaxNameLen+jsonEntryOverhead)

// jsonEntryOverhead is a map entry's framing (four quotes, colon, comma)
// plus the up-to-four bytes each value's own base64 padding adds over the
// whole set's encoded size.
const jsonEntryOverhead = 10

// providerSyncYAMLBudget bounds config.yaml and schema.yaml as encoded,
// plus the body's own JSON framing.
const providerSyncYAMLBudget = 1 << 20
