package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/sync/singleflight"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/dispatch/steering"
	"github.com/stperic/zzrouter/pkg/dispatch/wire"
	pfallback "github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	modelgroup "github.com/stperic/zzrouter/pkg/model/group"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/modelregistry"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/routing"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Proxy routes a request through a chain of candidate deployments with
// pre-stream fallback. When a deployment returns a retriable error
// (transport-level or HTTP 429/5xx) before the first byte is sent to the
// client, the proxy transparently retries with the next candidate.
type Proxy struct {
	deps              ServerDeps
	cooldowns         *pfallback.CooldownManager
	providerCooldowns *pfallback.CooldownManager
	strategies        *pfallback.StrategyRegistry
	loadTracker       *pfallback.LoadTracker
	latencyTracker    *pfallback.LatencyTracker
	healthChecker     *pfallback.HealthChecker

	// startGroup dedupes concurrent on-demand model starts. Key is the
	// model name (not (model, app)) — the pre-extraction behaviour was
	// to collapse starts across providers serving the same logical
	// model. Invariant #11.
	startGroup singleflight.Group
}

// New constructs a Proxy wired to its dependency surface and the
// fallback primitives (cooldowns, strategies, load tracker, latency
// tracker, health checker). latencyTracker is consulted by the
// X-Route-Latency-Budget-Ms hint; nil is safe (budget hint no-ops).
func New(
	deps ServerDeps,
	cooldowns, providerCooldowns *pfallback.CooldownManager,
	strategies *pfallback.StrategyRegistry,
	loadTracker *pfallback.LoadTracker,
	latencyTracker *pfallback.LatencyTracker,
	healthChecker *pfallback.HealthChecker,
) *Proxy {
	return &Proxy{
		deps:              deps,
		cooldowns:         cooldowns,
		providerCooldowns: providerCooldowns,
		strategies:        strategies,
		loadTracker:       loadTracker,
		latencyTracker:    latencyTracker,
		healthChecker:     healthChecker,
	}
}

// Start starts the health checker background probes. No-op if no health
// checker is configured. The Proxy owns the health checker's goroutine
// lifecycle; callers MUST NOT wrap Start in `go`.
func (p *Proxy) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if p.healthChecker != nil {
		p.healthChecker.Start(ctx)
	}
}

// Stop stops the health checker background probes. Accepts ctx for
// signature consistency with other lifecycle-managed subsystems;
// HealthChecker.Stop today is unconditional and does not consult it.
//
// KNOWN LIMITATION: startModelAsync goroutines spawned via go
// p.startModelAsync(...) are tied to a context.Background() with a
// 10-minute internal timeout — they are NOT cancelled by Stop. A Proxy
// teardown will leave in-flight on-demand starts running until they
// complete or time out. Wiring a Proxy-scoped context into these
// goroutines is tracked as Slice 6 follow-up work.
func (p *Proxy) Stop(_ context.Context) {
	if p == nil {
		return
	}
	if p.healthChecker != nil {
		p.healthChecker.Stop()
	}
}

// ProxyWithFallback tries deployments using the configured strategy
// until one succeeds. Deployments are filtered (health, cooldown, tags)
// then ordered by strategy. Each deployment is retried (with exponential
// backoff) before falling back to the next.
//
// body goes out as given apart from each deployment's model name: the
// chain serves every surface, so one whose body needs more (Chat's forced
// usage reporting) prepares it first. suppressUsageFrame drops the
// terminal usage-only frame the caller did not ask to see. The return
// names the provider of the deployment that answered, "" when none did.
func (p *Proxy) ProxyWithFallback(
	c *gin.Context,
	resolved *resolver.Resolved,
	body []byte,
	recorder *llm.InferenceRecorder,
	clientModel string,
	suppressUsageFrame bool,
) string {
	decisionStart := utils.Now()
	attempts := make([]pfallback.Attempt, 0, len(resolved.Candidates))
	var firstDeployment string

	// Parse per-call X-Route-* steering headers up front so an invalid
	// or unsupported header fails fast with a closed-enum 400 rather
	// than silently no-op'ing somewhere deep in the dispatch chain.
	hints, perr := steering.Parse(c)
	if perr != nil {
		p.writeSteeringError(c, perr)
		return ""
	}

	// Separate on-demand deployments that aren't running (start them async).
	admitted := make([]pfallback.Candidate, 0, len(resolved.Candidates))
	for _, dep := range resolved.Candidates {
		if err := p.deps.ValidateModel(c.Request.Context(), dep.Model); err != nil {
			attempts = append(attempts, pfallback.Attempt{DeploymentName: dep.Name, Err: err, Outcome: pfallback.OutcomeRetriable})
			continue
		}
		admitted = append(admitted, dep)
	}
	candidates, onDemandAttempts := p.separateOnDemand(admitted)
	attempts = append(attempts, onDemandAttempts...)

	// Filter: remove cooldown + unhealthy + tag-mismatched + steering-
	// rejected. Skipped is the parallel list of rejected candidates
	// with their closed-enum reason — surfaced to agents via
	// RoutingMetadata.Skipped at commit.
	filtered, skipped := pfallback.FilterDeployments(
		candidates,
		p.cooldowns,
		p.providerCooldowns,
		p.healthChecker,
		hints.RequireTags,
		p.latencyTracker,
		pfallback.SteeringHints{
			ExcludeReplicas: hints.ExcludeReplicas,
			LatencyBudgetMs: hints.LatencyBudgetMs,
		},
	)

	// Filter: remove candidates that don't have enough memory for the model.
	filtered = p.filterByMemory(filtered)

	// Apply routing strategy.
	strategy := p.strategies.Get(resolved.Strategy)
	if strategy == nil {
		strategy = p.strategies.Get(string(modelgroup.StrategyPriority)) // default
	}
	orderedDeps := strategy.Select(filtered)

	strategyName := resolved.Strategy
	if strategyName == "" {
		strategyName = string(modelgroup.StrategyPriority)
	}
	rctx := routeMetaCtx{
		clientModel:        clientModel,
		suppressUsageFrame: suppressUsageFrame,
		groupName:          resolved.GroupName,
		strategyUsed:       strategyName,
		preFilterSkip:      skipped,
		decisionStart:      decisionStart,
		steeringApplied:    hints.Applied(),
	}

	operation := string(genai.OperationName(c.FullPath()))

	for _, dep := range orderedDeps {
		// Track first actually-attempted deployment.
		if firstDeployment == "" {
			firstDeployment = dep.Name
		}

		// Track in-flight for least-load strategy.
		if p.loadTracker != nil {
			p.loadTracker.Acquire(dep.Name)
		}

		maxRetries := dep.MaxRetries
		if maxRetries <= 0 {
			maxRetries = pfallback.DefaultMaxRetries
		}

		rctx.attemptsSoFar = attempts
		depAttempt, disposition := p.tryDeployment(c, dep, body, recorder, resolved, maxRetries, rctx)
		attempts = append(attempts, depAttempt...)

		if p.loadTracker != nil {
			p.loadTracker.Release(dep.Name)
		}

		if disposition == attemptClientGone {
			p.recordFallbackMetrics(recorder, attempts, firstDeployment)
			return ""
		}

		if disposition == attemptCommitted {
			p.recordFallbackMetrics(recorder, attempts, firstDeployment)
			// Match the proxy's own local/remote test (deps.IsLocalNode)
			// so the metric tracks reality. A deployment tagged with the
			// local node's hostname dispatches locally; using a bare
			// `dep.Node != ""` heuristic would mislabel those as remote.
			outcome := routing.OutcomeLocal
			if dep.Node != "" && !p.deps.IsLocalNode(dep.Node) {
				outcome = routing.OutcomeRemote
			}
			// fallback_index = number of prior dispatch attempts; 0
			// means this was the first replica tried. attemptsSoFar
			// contains only the chain *before* this deployment.
			fbIdx := len(rctx.attemptsSoFar)
			routing.RecordFallbackDecision(c.Request.Context(),
				routing.StrategyFallback, outcome,
				string(genai.ProviderName(dep.App)), operation, fbIdx)

			wireChain, wireSkipped := buildRouteWireFields(attempts, rctx.preFilterSkip)
			routing.SetRouteSpanAttrs(c.Request.Context(),
				rctx.groupName, dep.Name, rctx.strategyUsed, len(wireChain), len(wireSkipped))
			return dep.App
		}
	}

	// All deployments exhausted.
	slog.Warn("All deployments exhausted", "group", resolved.GroupName, "attempts", len(attempts))
	p.recordFallbackMetrics(recorder, attempts, firstDeployment)
	// Provider is unknown when the chain exhausts — every candidate
	// failed, and the operator's question on this metric is "did we
	// fail to route at all?", not "which provider was last tried".
	routing.RecordFallbackDecision(c.Request.Context(),
		routing.StrategyFallback, routing.OutcomeNoBackend,
		"", operation, len(attempts))
	wireChain, wireSkipped := buildRouteWireFields(attempts, rctx.preFilterSkip)
	routing.SetRouteSpanAttrs(c.Request.Context(),
		rctx.groupName, "", rctx.strategyUsed, len(wireChain), len(wireSkipped))
	p.writeExhaustedResponse(c, resolved.GroupName, attempts)
	return ""
}

// buildRouteWireFields splits the request's attempt log into the two
// arrays carried by RoutingMetadata: real dispatch attempts go into
// fallback_chain, candidates skipped before they were tried (filter
// rejections + on-demand skips + cooldown skips) go into skipped.
// Stamping both lets agents see the full decision tree without needing
// to subscribe to side-channel events.
func buildRouteWireFields(attempts []pfallback.Attempt, preFilter []pfallback.Skip) ([]wire.FallbackAttempt, []wire.SkippedReplica) {
	var chain []wire.FallbackAttempt
	var skipped []wire.SkippedReplica
	for _, a := range attempts {
		switch a.Outcome {
		case pfallback.OutcomeCooldownSkip:
			skipped = append(skipped, wire.SkippedReplica{Replica: a.DeploymentName, Reason: "cooldown"})
		case pfallback.OutcomeOnDemandSkip:
			skipped = append(skipped, wire.SkippedReplica{Replica: a.DeploymentName, Reason: "on_demand"})
		default:
			chain = append(chain, wire.FallbackAttempt{
				Replica:    a.DeploymentName,
				Status:     a.StatusCode,
				DurationMs: a.Duration.Milliseconds(),
				Outcome:    a.Outcome,
			})
		}
	}
	for _, s := range preFilter {
		skipped = append(skipped, wire.SkippedReplica{
			Replica: s.Candidate.Name,
			Reason:  string(s.Reason),
		})
	}
	return chain, skipped
}

// routeMetaCtx carries the per-request context that tryDeployment needs
// to stamp route-level observability fields on the committed response.
// attemptsSoFar lets the commit-time metadata see prior deployments'
// attempts (e.g. the chain of replicas that failed before this one
// succeeded). preFilterSkip lists candidates that were never tried
// because the FilterDeployments pass rejected them.
type routeMetaCtx struct {
	// clientModel is the id the caller used. A replica answers in its
	// own model name, which is never the group alias the caller routed
	// through, so the response has to be put back into their spelling.
	clientModel string
	// suppressUsageFrame is set when usage was forced on for metering but
	// the caller never asked to see it, so the terminal usage frame is
	// dropped on the way out and their wire contract is unchanged.
	suppressUsageFrame bool
	groupName          string
	strategyUsed       string
	attemptsSoFar      []pfallback.Attempt
	preFilterSkip      []pfallback.Skip
	decisionStart      time.Time
	steeringApplied    []string
}

// attemptDisposition is what the chain does after one attempt.
type attemptDisposition uint8

const (
	attemptRetry attemptDisposition = iota
	attemptNextReplica
	attemptCommitted
	attemptClientGone
)

// attemptOutcome is one attempt's record and the chain's next move.
type attemptOutcome struct {
	attempt     pfallback.Attempt
	disposition attemptDisposition
}

// tryDeployment attempts one deployment, retrying it with backoff while no lease is held, and
// reports how the chain should go on.
func (p *Proxy) tryDeployment(
	c *gin.Context,
	dep pfallback.Candidate,
	body []byte,
	recorder *llm.InferenceRecorder,
	resolved *resolver.Resolved,
	maxRetries int,
	rctx routeMetaCtx,
) ([]pfallback.Attempt, attemptDisposition) {
	var attempts []pfallback.Attempt
	// The client sent the group alias; the backend needs the replica's own model name.
	if dep.Model != resolved.OriginalName {
		body = wire.RewriteRequestModel(body, c.Request.Header.Get("Content-Type"), dep.Model)
	}
	body = wire.SetDefaultFields(body, p.deps.RequestDefaults(dep.App, dep.Model, c.Request.URL.Path))
	for retry := 0; retry <= maxRetries; retry++ {
		attemptMeta := rctx
		attemptMeta.attemptsSoFar = append(append([]pfallback.Attempt{}, rctx.attemptsSoFar...), attempts...)
		outcome := p.tryOnce(c, dep, body, recorder, resolved, attemptMeta)
		attempts = append(attempts, outcome.attempt)
		if outcome.disposition != attemptRetry {
			return attempts, outcome.disposition
		}

		a := outcome.attempt
		if shouldRetry, backoff := pfallback.ShouldRetry(retry, maxRetries, a.StatusCode, a.Err); shouldRetry {
			utils.LogDebugf("[Fallback] %q retry %d/%d (status %d, backoff %s): %v", dep.Name, retry+1, maxRetries, a.StatusCode, backoff, a.Err)
			select {
			case <-time.After(backoff):
			case <-c.Request.Context().Done():
				return attempts, attemptClientGone
			}
			continue
		}

		cooldownDur, reason := pfallback.CooldownForStatus(a.StatusCode), pfallback.ReasonForStatus(a.StatusCode)
		if a.Err != nil {
			cooldownDur, reason = pfallback.DefaultCooldownDuration, pfallback.ReasonTransport
		}
		p.cooldowns.SetCooldown(dep.Name, cooldownDur, reason)
		p.providerCooldowns.SetCooldown(dep.App, cooldownDur, reason)
		utils.LogDebugf("[Fallback] %q retries exhausted (status %d, cooldown %s): %v", dep.Name, a.StatusCode, cooldownDur, a.Err)
		return attempts, attemptNextReplica
	}
	return attempts, attemptNextReplica
}

// tryOnce makes one attempt. A local instance is leased for the whole attempt, body relay
// included, and released before the caller backs off.
func (p *Proxy) tryOnce(
	c *gin.Context,
	dep pfallback.Candidate,
	body []byte,
	recorder *llm.InferenceRecorder,
	resolved *resolver.Resolved,
	rctx routeMetaCtx,
) attemptOutcome {
	start := utils.Now()
	outcome := attemptOutcome{attempt: pfallback.Attempt{DeploymentName: dep.Name, Outcome: pfallback.OutcomeRetriable}, disposition: attemptNextReplica}
	targetURL, up, inst, err := p.resolveDeploymentTarget(dep, c.Request.URL.Path, c.Request.URL.RawQuery)
	if err != nil {
		slog.Warn("Failed to resolve deployment target", "deployment", dep.Name, "error", err)
		outcome.attempt.Err, outcome.attempt.Duration = err, time.Since(start)
		return outcome
	}

	if inst != nil {
		lease, err := p.deps.LeaseInstance(c.Request, inst)
		if err != nil {
			outcome.attempt.Err, outcome.attempt.Duration = err, time.Since(start)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				outcome.attempt.Outcome, outcome.disposition = pfallback.OutcomeNonRetriable, attemptClientGone
			}
			return outcome
		}
		defer lease.Release()
		body = p.deps.InstanceBody(c.Request, lease, body)
	}
	recorder.SetUpstreamRequestData(body)

	var attemptCtx context.Context
	var cancel context.CancelFunc
	if dep.Timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(c.Request.Context(), dep.Timeout)
	} else {
		attemptCtx, cancel = context.WithCancel(c.Request.Context())
	}
	defer cancel()
	resp, err := p.deps.ProxyForwardDetached(attemptCtx, c.Request.Method, targetURL, attemptHeaders(c.Request.Header, up, dep), up, body)
	if err != nil {
		outcome.attempt.Err, outcome.attempt.Duration = err, time.Since(start)
		if pfallback.IsRetriable(0, err) {
			outcome.disposition = attemptRetry
			return outcome
		}
		outcome.attempt.Outcome, outcome.disposition = pfallback.OutcomeNonRetriable, attemptCommitted
		wire.RecordPreByteCopyError(recorder, "fallback_exhausted", err.Error())
		writeError(c, httperr.Error{Status: http.StatusBadGateway, Type: "api_error",
			Message: utils.SanitizeErrorMessage(err.Error()), Code: "fallback_exhausted"})
		return outcome
	}

	// Classify before retry/cooldown: a capability refusal is not evidence the deployment is unhealthy.
	if resp.StatusCode >= 400 {
		classifier := p.deps.FailureClassifier(dep.App)
		if inst != nil {
			classifier = inst.FailureClassifier
		}
		if up.IsCluster() {
			classifier = nil
		}
		httperr.NormalizeUpstreamResponse(resp, httperr.FromRequest(c.Request), classifier)
	}
	outcome.attempt.StatusCode = resp.StatusCode
	if pfallback.IsRetriable(resp.StatusCode, nil) {
		p.trackRateLimitHeaders(resp, dep)
		_ = resp.Body.Close()
		outcome.attempt.Duration, outcome.disposition = time.Since(start), attemptRetry
		return outcome
	}

	outcome.attempt.Duration, outcome.disposition = time.Since(start), attemptCommitted
	var result wire.Result
	if resp.StatusCode >= 400 {
		outcome.attempt.Outcome = pfallback.OutcomeNonRetriable
		result = wire.Commit(c.Request.Context(), c.Writer, resp, dep.App, recorder, p.deps.Normalizers(), p.deps.BufferedShape(wire.RoutingMetadata{Provider: dep.App}, recorder))
	} else {
		outcome.attempt.Outcome = pfallback.OutcomeSuccess
		p.cooldowns.ClearCooldown(dep.Name)
		p.providerCooldowns.ClearCooldown(dep.App)
		p.trackRateLimitHeaders(resp, dep)
		result = p.commitSuccess(c, dep, resp, recorder, resolved, rctx, outcome.attempt)
	}
	_ = resp.Body.Close()
	cancel()
	p.deps.FinishRelay(c.Writer, c.Request, resp, result)
	return outcome
}

// commitSuccess relays a successful response with the route's metadata.
func (p *Proxy) commitSuccess(c *gin.Context, dep pfallback.Candidate, resp *http.Response, recorder *llm.InferenceRecorder, resolved *resolver.Resolved, rctx routeMetaCtx, attempt pfallback.Attempt) wire.Result {
	// Test fakes pass a nil recorder.
	if recorder != nil {
		recorder.SetProvider(dep.App)
		recorder.SetModelGroup(resolved.GroupName, dep.Name)
		if dep.Node != "" {
			recorder.SetRouting(llm.RoutingDecisionRemote, dep.Node)
		} else {
			recorder.SetRouting(llm.RoutingDecisionLocal, p.deps.NodeName())
		}
	}

	// A cloud provider has no node.
	depNode := ""
	if resolved, ok := p.deps.Backend(dep.App); !ok || !resolved.Cloud {
		depNode = dep.Node
		if depNode == "" {
			depNode = p.deps.NodeName()
		}
	}

	injectUsage := p.deps.InjectUsageMetadata()
	fullAttempts := append([]pfallback.Attempt{}, rctx.attemptsSoFar...)
	fullAttempts = append(fullAttempts, attempt)
	chain, skipped := buildRouteWireFields(fullAttempts, rctx.preFilterSkip)
	meta := wire.RoutingMetadata{
		Provider:             dep.App,
		Deployment:           dep.Name,
		Node:                 depNode,
		InjectUsage:          injectUsage,
		ClientModel:          rctx.clientModel,
		GroupName:            rctx.groupName,
		StrategyUsed:         rctx.strategyUsed,
		FallbackChain:        chain,
		Skipped:              skipped,
		SteeringApplied:      rctx.steeringApplied,
		DecisionLatencyMs:    utils.Now().Sub(rctx.decisionStart).Milliseconds(),
		SuppressBodyMetadata: wire.SuppressBodyFromVerbose(c.Query("verbose")),
		SuppressUsageFrame:   rctx.suppressUsageFrame,
	}
	return wire.CommitWithRouting(c.Request.Context(), c.Writer, resp, meta, recorder, p.deps.Normalizers(), p.deps.BufferedShape(meta, recorder))
}

// separateOnDemand splits out on-demand deployments that aren't running.
// Running on-demand deployments pass through. Non-running ones are
// started async and skipped.
func (p *Proxy) separateOnDemand(deps []pfallback.Candidate) ([]pfallback.Candidate, []pfallback.Attempt) {
	ready := make([]pfallback.Candidate, 0, len(deps))
	var skipped []pfallback.Attempt

	for _, dep := range deps {
		if dep.OnDemand && dep.Node == "" && !p.isModelRunning(dep.Model) {
			// NOTE: startModelAsync is an owner-less goroutine — see
			// Stop() docstring. Pre-existing behaviour, preserved
			// verbatim in Slice 6.
			go p.startModelAsync(dep.Model, dep.App)
			utils.LogDebugf("[Fallback] Skipping %q: on-demand model not running, starting async", dep.Name)
			skipped = append(skipped, pfallback.Attempt{
				DeploymentName: dep.Name,
				Outcome:        pfallback.OutcomeOnDemandSkip,
			})
			continue
		}
		ready = append(ready, dep)
	}

	return ready, skipped
}

// writeSteeringError surfaces a steering-header parse failure as a 400
// with the closed-enum error body the rest of the routes surface uses.
// errors.Is(err, steering.ErrUnsupportedHeader) routes to a distinct
// code so agents can distinguish "header value invalid" from "header
// not yet supported on this build".
func (p *Proxy) writeSteeringError(c *gin.Context, perr *steering.ParseError) {
	code := "invalid_steering_header"
	if errors.Is(perr, steering.ErrUnsupportedHeader) {
		code = "unsupported_steering_header"
	}
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"errors": []gin.H{{
			"code":    code,
			"key":     perr.Header,
			"message": perr.Error(),
		}},
	})
}

// resolveDeploymentTarget returns the URL a deployment attempt goes to
// and what of the caller's request travels there.
func (p *Proxy) resolveDeploymentTarget(dep pfallback.Candidate, urlPath, rawQuery string) (string, backend.Upstream, *instance.Instance, error) {
	// 1. Cloud providers — always go to their API endpoint.
	resolved, hasEndpoint := p.deps.Backend(dep.App)
	if hasEndpoint && resolved.Cloud {
		return backend.PassthroughTarget(resolved.Endpoint, urlPath, rawQuery), resolved.Upstream, nil, nil
	}

	// 2. Remote node — its cluster port, over mTLS.
	if dep.Node != "" && !p.deps.IsLocalNode(dep.Node) {
		base := p.deps.ClusterURL(dep.Node)
		if base == "" {
			return "", backend.Upstream{}, nil, fmt.Errorf("node %q advertises no cluster port", dep.Node)
		}
		return backend.PassthroughTarget(base, urlPath, rawQuery), backend.Cluster(), nil, nil
	}

	// 3. On-demand local model — use running instance port.
	if dep.OnDemand {
		if inst, found := p.deps.AppInstance(dep.Model); found {
			base := fmt.Sprintf("http://%s:%d", instanceBackendNode(inst), inst.Port)
			return backend.PassthroughTarget(base, urlPath, rawQuery), backend.Engine(), inst, nil
		}
		return "", backend.Upstream{}, nil, fmt.Errorf("on-demand model %q not running", dep.Model)
	}

	// 4. Local provider endpoint (e.g., ollama on localhost:11434).
	if hasEndpoint {
		return backend.PassthroughTarget(resolved.Endpoint, urlPath, rawQuery), resolved.Upstream, nil, nil
	}

	return "", backend.Upstream{}, nil, fmt.Errorf("cannot resolve target for deployment %q (app=%s)", dep.Name, dep.App)
}

// attemptHeaders returns the headers an attempt sends. A worker is told
// which provider serves the replica, as a direct dispatch tells it, so it
// does not choose another one for the same model.
func attemptHeaders(h http.Header, up backend.Upstream, dep pfallback.Candidate) http.Header {
	if !up.IsCluster() || dep.App == "" {
		return h
	}
	h = h.Clone()
	h.Set(constants.HeaderServingProvider, dep.App)
	return h
}

// isModelRunning checks if an on-demand model is currently running
// locally.
func (p *Proxy) isModelRunning(model string) bool {
	inst, found := p.deps.AppInstance(model)
	return found && inst.GetStatus() == instance.StatusRunning
}

// filterByMemory removes candidates on nodes that don't have enough
// memory for the model. Uses the node resource cache (refreshed
// alongside the model cache) — no per-request calls. Only checks
// on-demand models that aren't already running (running models have
// memory allocated). Rough first-pass: estimates model memory from
// parameter count, compares against cached node metrics.
func (p *Proxy) filterByMemory(candidates []pfallback.Candidate) []pfallback.Candidate {
	if !p.deps.NodeCacheReady() || len(candidates) <= 1 {
		return candidates
	}

	// All candidates in a model group resolve to the same underlying
	// model, so one estimate covers all deployments.
	modelName := candidates[0].Model
	estimate := modelregistry.EstimateModelMemory(modelName, "", "")
	if estimate == nil || estimate.MinMemoryMB <= 0 {
		return candidates // Can't estimate — pass through.
	}

	localHostname := p.deps.NodeName()

	result := make([]pfallback.Candidate, 0, len(candidates))
	for _, c := range candidates {
		// Already-running models have memory allocated — pass through.
		if c.Node == "" && p.isModelRunning(c.Model) {
			result = append(result, c)
			continue
		}

		// Resolve node name for cache lookup.
		nodeName := c.Node
		if nodeName == "" {
			nodeName = localHostname
		}

		metrics := p.deps.NodeMetrics(nodeName)
		if metrics == nil {
			result = append(result, c) // No metrics for this node — don't filter.
			continue
		}

		// Check available memory: prefer GPU VRAM if available, fall
		// back to RAM.
		availableMB := metrics.RAMAvailableMB
		if metrics.GPUMemoryFreeMB > 0 {
			availableMB = metrics.GPUMemoryFreeMB
		}

		if availableMB < estimate.MinMemoryMB {
			slog.Info("[Fallback] Skipping deployment: insufficient memory",
				"deployment", c.Name, "node", nodeName, "model", modelName,
				"required_mb", estimate.MinMemoryMB, "available_mb", availableMB)
			continue
		}

		result = append(result, c)
	}

	// Never filter ALL candidates — keep at least one to try (let it
	// fail naturally).
	if len(result) == 0 && len(candidates) > 0 {
		return candidates[:1]
	}

	return result
}

// startModelAsync triggers an async model start without blocking the
// request. Uses singleflight to deduplicate concurrent starts for the
// same model.
//
// LIFECYCLE NOTE: This goroutine is owner-less with respect to Proxy.Stop.
// The internal context.Background() + 10min timeout is the only bound on
// runtime. Preserved verbatim from the pre-extraction code. Wiring a
// Proxy-scoped context is tracked as Slice 6 follow-up.
func (p *Proxy) startModelAsync(model, app string) {
	// Inner func always returns (nil, nil); outer error is always nil.
	_, _, _ = p.startGroup.Do(model, func() (any, error) {
		slog.Info("Starting on-demand model asynchronously", "model", model, "provider", app)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		exec := p.deps.NewLoadExecutor()
		if err := exec(ctx, model, app); err != nil {
			slog.Error("Failed to start on-demand model", "model", model, "error", err)
		}
		return nil, nil
	})
}

// recordFallbackMetrics updates the inference recorder with fallback
// routing data.
func (p *Proxy) recordFallbackMetrics(recorder *llm.InferenceRecorder, attempts []pfallback.Attempt, firstDeployment string) {
	if recorder == nil {
		return
	}

	// Count actual attempts (exclude skips).
	tried := 0
	for _, a := range attempts {
		if a.Outcome != pfallback.OutcomeCooldownSkip && a.Outcome != pfallback.OutcomeOnDemandSkip {
			tried++
		}
	}

	if tried > 1 || (tried == 0 && len(attempts) > 0) {
		recorder.SetFallback(tried, firstDeployment)
	}
}

// modelLoadingRetryAfter is how long to tell a caller to wait while an
// on-demand model starts: a cold start takes tens of seconds.
const modelLoadingRetryAfter = 60 * time.Second

// writeExhaustedResponse writes a 503 when all deployments are exhausted.
func (p *Proxy) writeExhaustedResponse(c *gin.Context, groupName string, attempts []pfallback.Attempt) {
	if len(attempts) > 0 {
		allConflicts := true
		for _, a := range attempts {
			allConflicts = allConflicts && errors.Is(a.Err, config.ErrModelNameConflict)
		}
		if allConflicts {
			writeError(c, httperr.Error{Status: http.StatusConflict, Type: "invalid_request_error", Code: "model_name_conflict", Message: attempts[0].Err.Error()})
			return
		}
	}
	hasOnDemandPending := false
	for _, a := range attempts {
		if a.Outcome == pfallback.OutcomeOnDemandSkip {
			hasOnDemandPending = true
			break
		}
	}

	message := fmt.Sprintf("No available deployments for model %q. All backends are unavailable, in cooldown, or lack sufficient resources.", groupName)
	code := "model_not_available"
	var retryAfter time.Duration

	if hasOnDemandPending {
		message = fmt.Sprintf("Model %q is starting up. Please retry in a few seconds.", groupName)
		code = "model_loading"
		retryAfter = modelLoadingRetryAfter
	}

	writeError(c, httperr.Error{Status: http.StatusServiceUnavailable, Type: "server_error", Message: message, Code: code, RetryAfter: retryAfter})
}

// writeError writes e in the dialect of c's route group. Every routed
// request carries a responder (httperr.AttachByPath); without one there
// is no dialect to speak, so only the status goes out.
func writeError(c *gin.Context, e httperr.Error) {
	r := httperr.FromContext(c)
	if r == nil {
		c.AbortWithStatus(e.Status)
		return
	}
	r.WriteError(c.Writer, c.Request, e)
}

// trackRateLimitHeaders parses x-ratelimit-* headers from a response and
// stores the snapshot. If remaining requests or tokens hit 0, sets a
// proactive cooldown using the provider's own reset duration instead of
// guessing.
func (p *Proxy) trackRateLimitHeaders(resp *http.Response, dep pfallback.Candidate) {
	// Store snapshot via the shared host-level hook.
	p.deps.TrackProviderRateLimit(dep.App, resp)

	snap := pfallback.ParseRateLimitHeaders(resp.Header)
	if snap == nil {
		return
	}

	// Proactive cooldown: if remaining requests or tokens hit 0, cool
	// down using the provider's own reset duration (much more accurate
	// than guessing).
	if snap.RemainingRequests == 0 && snap.LimitRequests > 0 && snap.ResetRequests > 0 {
		p.providerCooldowns.SetCooldown(dep.App, snap.ResetRequests, pfallback.ReasonRateLimit)
		utils.LogDebugf("[RateLimit] %q requests exhausted, proactive cooldown %s", dep.App, snap.ResetRequests)
	}
	if snap.RemainingTokens == 0 && snap.LimitTokens > 0 && snap.ResetTokens > 0 {
		// Only extend cooldown (tokens might reset later than requests).
		p.providerCooldowns.SetCooldownIfLonger(dep.App, snap.ResetTokens, pfallback.ReasonRateLimit)
		utils.LogDebugf("[RateLimit] %q tokens exhausted, proactive cooldown %s", dep.App, snap.ResetTokens)
	}
}

// instanceBackendNode extracts the backend host from instance. Returns
// constants.Localhost for local instances, or the remote host from
// HealthURL for remote instances.
//
// Shares the `constants.Localhost` anchor with server's
// getInstanceBackendNode so both copies move together if the project
// ever repoints the local-host token.
func instanceBackendNode(inst *instance.Instance) string {
	if inst.HealthURL != "" && !strings.Contains(inst.HealthURL, constants.Localhost) {
		if u, err := url.Parse(inst.HealthURL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return constants.Localhost
}
