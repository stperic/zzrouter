package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/model/resolver"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/routing"
)

// routingStrategy returns the routing-strategy label that captures which
// router component made the dispatch decision for traffic flowing through
// this node. Workers run a local-only dispatcher (their resolver returns
// only local candidates); coordinators and standalone nodes use the
// cluster-aware path. The fallback strategy is emitted from inside
// pkg/dispatch/chain when a request takes the fallback proxy path.
//
// Only LLM-facing dispatch paths consult this helper — cluster
// control-plane traffic (`/zzrouter/v1/internal/*`) and Ollama
// management commands (`/api/show`, `/api/copy`, `/api/delete`,
// `/api/push`) deliberately don't emit a routing decision.
func (s *Server) routingStrategy() routing.Strategy {
	if s.cluster.listener != nil && s.cluster.listener.Mode() == clusternode.Worker {
		return routing.StrategyLocalOnly
	}
	return routing.StrategyClusterAware
}

// recordErrorOnContext pulls an InferenceRecorder from the request
// context (if any) and tags it with the given error_type so the OTel
// GenAI series carries the error.type label. Calls RecordCompletion(0,0)
// because pre-backend abort paths never reach the wire layer that
// would otherwise emit. Safe no-op when no recorder is attached.
//
// Only call this on PRE-BACKEND failure paths — anything that aborts
// before s.proxy.* / s.streamFromInstance is invoked. Calling it on a
// path that ALSO hits the wire layer would double-emit the
// operation_duration histogram.
func recordErrorOnContext(ctx context.Context, errType, message string) {
	rec, _ := ctx.Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
	if rec == nil {
		return
	}
	rec.SetError(errType, message)
	rec.RecordCompletion(0, 0)
}

// requestRecorder returns nil for a request marked CtxKeyNotInference
// (every recorder method is a no-op on nil, so it reaches neither the
// inference log nor the GenAI request counts; routing and inflight
// signals still see it). Otherwise it returns the recorder the handler
// stashed, or a fresh one so a proxied request is logged even when its
// handler made none.
func requestRecorder(ctx context.Context, model, provider string) *llm.InferenceRecorder {
	if notInference, _ := ctx.Value(CtxKeyNotInference).(bool); notInference {
		return nil
	}
	if rec, _ := ctx.Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder); rec != nil {
		rec.SetProvider(provider)
		return rec
	}
	return llm.NewInferenceRecorder(ctx, model, provider)
}

// headerNotInference carries markNotInference across a cluster hop. Every
// hop copies the request's headers, so a worker learns it however the
// request reaches it; only the worker's cluster engine, which a
// coordinator alone can reach, believes it (clusterHeadersFromCoordinator),
// and every client-facing engine drops a copy a client sent
// (dropCoordinatorHeaders).
const headerNotInference = "X-Zzrouter-Not-Inference"

// markNotInference keeps a request that is routed like inference but runs
// none out of the inference log, on this node and any it is proxied to.
func markNotInference(c *gin.Context) {
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), CtxKeyNotInference, true))
	c.Request.Header.Set(headerNotInference, "1")
}

// stripNodeHint removes the "@node" routing hint from the model string
// and re-synchronizes every copy of the request body that downstream
// code reads: the local slice, c.Request.Body, and the context stash.
// The stash matters because proxyToInstance rebuilds the outbound body
// from it — leaving the hint there puts it back on the wire, where an
// engine that validates model names (Ollama) rejects the request. The
// returned node hint is empty when the model carried none.
func (s *Server) stripNodeHint(c *gin.Context, modelName string, body []byte) (string, []byte, string) {
	cleanModel, nodeHint := s.parseInferenceModel(c, modelName)
	if cleanModel == modelName {
		return modelName, body, nodeHint
	}
	body = rewriteModelInBody(body, cleanModel)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	stashInferenceContext(c, cleanModel, body)
	// Everything downstream now sees the cleaned name, which is what the
	// engine needs. Keep the client's own spelling so the response can be
	// answered in it: the decorated form is the id the catalog advertises,
	// and a caller that indexes on it should get it back.
	c.Request = c.Request.WithContext(
		context.WithValue(c.Request.Context(), CtxKeyClientModel, modelName))
	return cleanModel, body, nodeHint
}

// requestUsageCounts asks the engine to report token counts, returning the
// body to dispatch.
//
// An OpenAI-shape stream carries usage only when the caller opts in, so
// without this the inference log recorded 0 in / 0 out for every stream whose
// client did not ask — while the same model over /api/* logged real counts,
// because Ollama's final frame always carries them. Anything summing that
// store undercounts streaming traffic specifically, which is the expensive
// kind.
//
// Chat and its group route both prepare the body here before dispatch;
// chain.ProxyWithFallback sends what it is given, since it serves surfaces
// whose bodies have no stream_options.
//
// Usage is forced on rather than defaulted: a caller sending
// include_usage:false would otherwise log 0 in / 0 out, and budget settlement
// spends against that store — so opting out of usage was opting out of being
// billed. What the caller SEES is a separate question, answered by stashing
// their intent for the response path, which strips the terminal usage frame
// when they never asked for it.
func requestUsageCounts(c *gin.Context, body []byte) []byte {
	forced, callerAsked := forceUsageReporting(body)
	if !callerAsked {
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), CtxKeySuppressUsageFrame, true))
	}
	if bytes.Equal(forced, body) {
		return body
	}
	// Leaving the stash on the pre-forced bytes put the caller's original
	// stream_options back on the wire. The engine then sent no usage
	// frame and a local streaming request logged 0 in / 0 out while
	// looking perfectly healthy.
	replaceRequestBody(c, forced)
	return forced
}

// replaceRequestBody makes body the request's outbound bytes in both
// places dispatch reads them: c.Request.Body, and the context stash that
// proxyToInstance rebuilds the outbound body from. Updating only the
// first puts the old bytes on the wire.
func replaceRequestBody(c *gin.Context, body []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request = c.Request.WithContext(
		context.WithValue(c.Request.Context(), CtxKeyOriginalBody, body))
}

// suppressUsageFrameFromContext reports whether the terminal usage frame must
// be dropped before the response reaches the caller. Set only by the hop that
// faces the original client: a cluster-routed request arrives at the worker
// already carrying include_usage:true, so the worker reads "caller asked",
// leaves the frame alone, and the coordinator does both the counting and the
// stripping.
func suppressUsageFrameFromContext(ctx context.Context) bool {
	suppress, _ := ctx.Value(CtxKeySuppressUsageFrame).(bool)
	return suppress
}

// attributeServingNode records which node will answer, on the REQUEST.
//
// The response header cannot be set here: it has to land after the backend's
// own headers are copied, or the merge overwrites it. So the node rides the
// request instead — the proxy layer reads it back on the way out, and a
// cluster-routed request carries it to the worker, which echoes it on its own
// response for the coordinator to merge.
//
// Every non-group dispatch path calls this. Setting it unconditionally also means an
// inbound X-zzrouter-Node from a client is overwritten rather than echoed
// back as if zzRouter had asserted it.
func (s *Server) attributeServingNode(c *gin.Context, resolved *resolver.Resolved) {
	if resolved.IsRemote() {
		c.Request.Header.Set(constants.HeaderServingNode, resolved.Node)
		return
	}
	c.Request.Header.Set(constants.HeaderServingNode, s.node.Nodename())
}

// clientModelFromContext returns the model id the caller used, when it
// differs from the token sent to the engine. Empty when the caller's id
// was already the wire id, in which case nothing needs restoring.
func clientModelFromContext(ctx context.Context) string {
	name, _ := ctx.Value(CtxKeyClientModel).(string)
	return name
}

// callerModel is the id a response must be answered in: the caller's own
// spelling when they decorated it with an @node hint, and otherwise the
// group alias they routed through. Empty means the caller's id is already
// what the engine will echo, so nothing needs restoring.
func callerModel(ctx context.Context, resolved *resolver.Resolved) string {
	if m := clientModelFromContext(ctx); m != "" {
		return m
	}
	if resolved == nil {
		return ""
	}
	return resolved.GroupName
}

// enforceAndAttribute is the access gate every inference surface runs:
// it labels the request with the caller's identity, applies the full
// chain (suspended → expired → model access → RPM → TPM → budget →
// concurrency), and hands back the teardown the request owes.
//
// It reports whether the request may proceed; on false the denial has
// already been written, in the dialect of whichever surface is in scope.
// release is always safe to defer, including on the deny path.
//
// Every dispatcher goes through here. A surface that resolves models on
// its own instead is a surface with no quotas, no model access, and no
// spend attribution — which is what /api/* silently was.
func (s *Server) enforceAndAttribute(c *gin.Context, modelName string, recorder *llm.InferenceRecorder) (release func(), ok bool) {
	// Key + team IDs feed post-response settlement and log attribution.
	// LiteLLM-vocabulary caller labels (api_key_alias, hashed_api_key,
	// team_alias) are mirrored at recorder construction by
	// mirrorCallerIdentityToRecorder, so the bridge has them regardless
	// of which dispatch surface the request flowed through.
	if ac := GetAccessContext(c); recorder != nil && ac != nil && ac.Key != nil && ac.Key.IsVirtual {
		recorder.SetKeyID(ac.Key.ID)
		if ac.Team != nil {
			recorder.SetTeamID(ac.Team.ID)
		}
	}

	if !s.access.Enforce(c, modelName) {
		return func() {}, false
	}

	// Mirror the reservation ID that Enforce stashed on gin onto the
	// recorder so the inference-log bridge settles the exact stash this
	// request created. A dispatcher with no recorder falls back to the
	// reaper; spend simply does not settle for that request.
	s.access.MirrorReservationIDToRecorder(c, recorder)

	// Order matches the LIFO the two separate defers used to produce:
	// refund an unsettled reservation, then drop the concurrency slot.
	return func() {
		s.access.CancelPendingReservationIfUnsettled(c)
		ReleaseConcurrency(c)
	}, true
}

// resolveAndDispatchWithRecorder resolves a model and dispatches the request,
// updating the inference recorder and span with routing decisions.
// Used by instrumented handlers (chat completions, completions).
func (s *Server) resolveAndDispatchWithRecorder(c *gin.Context, modelName string, body []byte, recorder *llm.InferenceRecorder, span trace.Span, endpoint string) {
	// Extract @node hint from the model string. Header X-Node still wins.
	modelName, body, nodeHint := s.stripNodeHint(c, modelName, body)

	release, ok := s.enforceAndAttribute(c, modelName, recorder)
	defer release()
	if !ok {
		return
	}

	if err := s.validateModel(c.Request.Context(), modelName); err != nil {
		llm.SetSpanError(span, err)
		recorder.SetError("model_admission_failed", err.Error())
		recorder.RecordCompletion(0, 0)
		writeModelAdmissionError(c.Writer, c.Request, err)
		return
	}
	resolved, err := s.model.Resolver.Resolve(c.Request.Context(), modelName, nodeHint)
	if err != nil {
		llm.SetSpanError(span, err)
		recorder.SetError("model_resolve_failed", err.Error())
		recorder.RecordCompletion(0, 0)
		// Provider is unknown on resolve failure — emit empty so the
		// no-backend bucket lights up regardless of which provider the
		// caller asked for.
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeNoBackend,
			"", string(genai.OperationName(c.FullPath())))
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "Failed to resolve model"})
		return
	}

	if !s.admitResolvedModels(c, resolved, nodeHint) || !s.admitImages(c, modelName, body, resolved) {
		return
	}
	body = requestUsageCounts(c, body)

	// Route through fallback proxy if deployments exist (model group with fallback chain)
	// Note: fallback proxy handles per-deployment model name rewriting internally
	// Uses > 0 (not > 1) so single-deployment groups also get cooldown tracking
	if s.providers.fallback != nil && len(resolved.Candidates) > 0 {
		// Routing-decisions emit (strategy=fallback) lives inside the
		// fallback proxy itself — it has the per-deployment view and
		// can tag remote/local/no_backend after the chain settles.
		span.SetAttributes(llm.RoutingDecision(llm.RoutingDecisionFallback))
		s.providers.fallback.ProxyWithFallback(c, resolved, body, recorder,
			callerModel(c.Request.Context(), resolved), suppressUsageFrameFromContext(c.Request.Context()))
		llm.SetSpanOK(span)
		return
	}

	// Record model group routing info (single deployment, no fallback needed)
	if resolved.GroupName != "" && len(resolved.Candidates) > 0 {
		recorder.SetModelGroup(resolved.GroupName, resolved.Candidates[0].Name)
	}

	// Rewrite model name in body/request when resolved via model group
	// (client sends group alias like "fast-chat", backend needs real model like "llama3:latest")
	if resolved.GroupName != "" && resolved.ModelName != modelName {
		body = rewriteModelInBody(body, resolved.ModelName)
		replaceRequestBody(c, body)
		// The caller asked for the group, so the group is what the answer
		// should name. Which replica served it is reported by the
		// zzrouter block; overwriting the caller's own handle with a
		// replica name it never mentioned is the same lie the @node path
		// used to tell.
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), CtxKeyClientModel, modelName))
	}

	s.attributeServingNode(c, resolved)

	operation := string(genai.OperationName(c.FullPath()))
	provider := string(genai.ProviderName(resolved.Provider))
	if resolved.IsRemote() {
		span.SetAttributes(
			llm.RoutingDecision(llm.RoutingDecisionRemote),
			llm.RoutingNode(resolved.Node),
			llm.ModelProvider(resolved.Provider),
		)
		recorder.SetRouting(llm.RoutingDecisionRemote, resolved.Node)
		recorder.SetProvider(resolved.Provider)
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeRemote, provider, operation)
		s.proxyToRemoteNode(c, resolved.Node, resolved.Provider, body)
		return
	}

	span.SetAttributes(llm.RoutingDecision(llm.RoutingDecisionLocal))
	recorder.SetRouting(llm.RoutingDecisionLocal, s.node.Nodename())
	routing.RecordDecision(c.Request.Context(),
		s.routingStrategy(), routing.OutcomeLocal, provider, operation)
	if err := s.HandleLocalModel(c.Writer, c.Request, resolved.ModelName, endpoint); err != nil {
		llm.SetSpanError(span, err)
		return
	}
	llm.SetSpanOK(span)
}

// resolveAndDispatchSimple enforces access and quota, then resolves a
// model and dispatches the body unchanged: no span, and no body rewrite
// beyond the model name. A handler that stashed an inference recorder on
// the request context (CtxKeyInferenceRecorder) gets key, team and
// reservation attribution on it, so spend settles; without one the proxy
// sites log a keyless row.
func (s *Server) resolveAndDispatchSimple(c *gin.Context, modelName string, body []byte, endpoint string) {
	modelName, body, nodeHint := s.stripNodeHint(c, modelName, body)

	recorder, _ := c.Request.Context().Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
	release, ok := s.enforceAndAttribute(c, modelName, recorder)
	defer release()
	if !ok {
		return
	}
	resolved, ok := s.resolveTarget(c, modelName, nodeHint)
	if !ok {
		return
	}
	if !s.admitImages(c, modelName, body, resolved) {
		return
	}
	s.dispatchTo(c, resolved, modelName, body, endpoint)
}

// resolveTarget resolves modelName for dispatch. On false the failure
// has been written.
func (s *Server) resolveTarget(c *gin.Context, modelName, nodeHint string) (*resolver.Resolved, bool) {
	if err := s.validateModel(c.Request.Context(), modelName); err != nil {
		writeModelAdmissionError(c.Writer, c.Request, err)
		return nil, false
	}
	resolved, err := s.model.Resolver.Resolve(c.Request.Context(), modelName, nodeHint)
	if err != nil {
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeNoBackend, "", string(genai.OperationName(c.FullPath())))
		recordErrorOnContext(c.Request.Context(), "server_error", "Failed to resolve model")
		writeError(c.Writer, c.Request, httperr.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "Failed to resolve model"})
		return nil, false
	}
	return resolved, s.admitResolvedModels(c, resolved, nodeHint)
}

// dispatchTo routes an admitted request to resolved: a model group
// through the fallback chain, anything else to its node or local
// instance. The body goes out as given apart from the model name; a
// surface whose body needs more prepares it before calling.
func (s *Server) dispatchTo(c *gin.Context, resolved *resolver.Resolved, modelName string, body []byte, endpoint string) {
	ctx := c.Request.Context()
	if s.providers.fallback != nil && len(resolved.Candidates) > 0 {
		recorder, _ := ctx.Value(CtxKeyInferenceRecorder).(*llm.InferenceRecorder)
		served := s.providers.fallback.ProxyWithFallback(c, resolved, body, recorder,
			callerModel(ctx, resolved), false)
		s.stashServingProvider(c, served)
		return
	}

	if resolved.GroupName != "" && resolved.ModelName != modelName {
		body = rewriteModelInBody(body, resolved.ModelName)
		replaceRequestBody(c, body)
	}
	s.stashServingProvider(c, resolved.Provider)
	s.attributeServingNode(c, resolved)

	operation := string(genai.OperationName(c.FullPath()))
	provider := string(genai.ProviderName(resolved.Provider))
	if resolved.IsRemote() {
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeRemote, provider, operation)
		s.proxyToRemoteNode(c, resolved.Node, resolved.Provider, body)
		return
	}

	routing.RecordDecision(c.Request.Context(),
		s.routingStrategy(), routing.OutcomeLocal, provider, operation)
	_ = s.HandleLocalModel(c.Writer, c.Request, resolved.ModelName, endpoint)
}

// narrowToWireEndpoint keeps the targets that serve an endpoint, natively
// or through its translation shim (compat; "" when it has none), and
// reports which way they serve it. A model group whose replicas differ is
// narrowed to one way, native when it has any such replica, so a request
// is translated once or not at all, never per replica. A target left with
// neither is refused in the request's dialect. A provider the config does
// not know passes as native; dispatch answers for it.
func (s *Server) narrowToWireEndpoint(c *gin.Context, model string, resolved *resolver.Resolved, native, compat string) (wireEndpointMode, bool) {
	servedAs := func(mode wireEndpointMode) wireEndpointMode {
		if mode == wireModeUnknown {
			return wireModeNative
		}
		return mode
	}
	if len(resolved.Candidates) > 0 {
		byMode := map[wireEndpointMode][]fallback.Candidate{}
		for _, cand := range resolved.Candidates {
			mode := servedAs(s.targetWireMode(c.Request.Context(), cand.Model, cand.App, cand.Node, native, compat))
			byMode[mode] = append(byMode[mode], cand)
		}
		for _, mode := range []wireEndpointMode{wireModeNative, wireModeCompat} {
			if kept := byMode[mode]; len(kept) > 0 {
				// The direct target is the group's first replica, which may
				// be one just dropped.
				resolved.Candidates = kept
				resolved.ModelName, resolved.Node, resolved.Provider = kept[0].Model, kept[0].Node, kept[0].App
				return mode, true
			}
		}
	} else if mode := servedAs(s.targetWireMode(c.Request.Context(), resolved.ModelName, resolved.Provider, resolved.Node, native, compat)); mode != wireModeUnsupported {
		return mode, true
	}
	declare := fmt.Sprintf("%q", native)
	if compat != "" {
		declare = fmt.Sprintf("%q or %q", native, compat)
	}
	httperr.FromContext(c).BadRequest(c, fmt.Sprintf("model %q is served by a provider that declares neither %s "+
		"in capabilities.wire_endpoints (a shipped provider takes that list from its template; "+
		"an operator-added one can declare it)", model, declare), "model")
	return wireModeUnsupported, false
}

// stashServingProvider records which provider serves the request so
// post-dispatch observers (the /v1/responses affinity recorder) need not
// re-run resolution.
func (s *Server) stashServingProvider(c *gin.Context, provider string) {
	if provider == "" {
		return
	}
	c.Request = c.Request.WithContext(
		context.WithValue(c.Request.Context(), CtxKeyProvider, provider))
}
