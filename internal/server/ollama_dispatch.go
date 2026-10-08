// Ollama inference dispatch. Shares the /v1/* pipeline (resolver + fallback
// proxy + strategy) so /api/chat, /api/generate, and /api/embed get the same
// load balancing, failover, and auto-group behavior as OpenAI-compat endpoints.
//
// The Ollama-provider-scope invariant is enforced here by filtering resolved
// deployments to Ollama providers at the protocol-surface boundary, before
// the fallback proxy dispatches. Non-Ollama models return a 404 pointing at
// /v1/chat/completions, matching the pre-refactor contract.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/routing"
)

// dispatchOllamaInference resolves an Ollama-compat inference request and
// routes it through the shared dispatch pipeline. On success, the HTTP
// response is written to c.Writer. On failure, a *RoutedError is returned
// for the handler layer to translate to Ollama's flat {"error":"..."} shape.
//
// endpointType is the Ollama route the request entered through —
// "chat", "generate", "embeddings", or "embed". Threaded down to the
// recorder so OTel emission lands on the right operation_name. Empty
// or unknown defaults to chat (preserves prior behavior for callers
// that haven't been updated).
//
// Behavior per resolution outcome:
//   - Resolved to a model group with ≥1 Ollama deployment after filtering →
//     hand off to FallbackProxy (strategy + health + failover).
//   - Resolved to a single Ollama deployment, remote → proxy to that node.
//   - Resolved to a single Ollama deployment, local → forward to local
//     Ollama endpoint.
//   - Resolved to a non-Ollama provider (or group with no Ollama replicas) →
//     404 with the canonical "use /v1/chat/completions" message.
//
// Ollama daemon serves all endpoints from one server, so per-endpoint
// launch keying is meaningless here — endpointType only feeds the
// recorder, never the dispatch decision.
func (s *Server) dispatchOllamaInference(c *gin.Context, body []byte, modelName, endpointType string) error {
	// Extract @node hint from the model string. Header X-Node still wins.
	modelName, body, nodeHint := s.stripNodeHint(c, modelName, body)

	// Create and stash an inference recorder up front so downstream proxies
	// and the fallback proxy can attach provider/routing/usage metadata.
	// The recorder is consumed by InferenceLogBridge on response completion.
	recorder := newOllamaInferenceRecorder(c, modelName, body, endpointType)

	release, ok := s.enforceAndAttribute(c, modelName, recorder)
	defer release()
	if !ok {
		// Enforce wrote the denial in the Ollama dialect already.
		return nil
	}

	if err := s.validateModel(c.Request.Context(), modelName); err != nil {
		writeModelAdmissionError(c.Writer, c.Request, err)
		return nil
	}
	resolved, err := s.model.Resolver.Resolve(c.Request.Context(), modelName, nodeHint)
	if err != nil {
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeNoBackend,
			"", string(genai.OperationName(c.FullPath())))
		return newProblemError(http.StatusNotFound, "Not Found",
			fmt.Sprintf("model %q not found", modelName))
	}

	// Enforce the /api/* Ollama-provider-scope invariant.
	origCandidateCount := len(resolved.Candidates)
	resolved.Candidates = filterCandidatesByProvider(resolved.Candidates, constants.AppOllama)

	// Group case: at least one Ollama replica → load-balanced dispatch.
	if len(resolved.Candidates) > 0 {
		if !s.admitResolvedModels(c, resolved, nodeHint) || !s.admitImages(c, modelName, body, resolved) {
			return nil
		}
		body = requestUsageCounts(c, body)
		s.providers.fallback.ProxyWithFallback(c, resolved, body, recorder,
			callerModel(c.Request.Context(), resolved), suppressUsageFrameFromContext(c.Request.Context()))
		return nil
	}

	// A group existed but held no Ollama replicas after filtering → the
	// model is known but not reachable via /api/*.
	if origCandidateCount > 0 {
		return ollamaUnavailableErr(modelName)
	}

	// Single-deployment resolution (no group). Reject non-Ollama providers.
	if resolved.Provider != "" && resolved.Provider != constants.AppOllama {
		return ollamaUnavailableErr(modelName)
	}

	if !s.admitResolvedModels(c, resolved, nodeHint) || !s.admitImages(c, modelName, body, resolved) {
		return nil
	}
	// The Ollama surface never set this, so /api/* answered with no node at
	// all. Group dispatch returns above and is attributed by CommitWithRouting.
	s.attributeServingNode(c, resolved)

	operation := string(genai.OperationName(c.FullPath()))
	// Remote Ollama deployment → proxy to that node.
	if resolved.IsRemote() {
		recorder.SetProvider(resolved.Provider)
		recorder.SetRouting(llm.RoutingDecisionRemote, resolved.Node)
		routing.RecordDecision(c.Request.Context(),
			s.routingStrategy(), routing.OutcomeRemote,
			string(genai.ProviderName(resolved.Provider)), operation)
		s.proxyToRemoteNode(c, resolved.Node, resolved.Provider, body)
		return nil
	}

	// Local coordinator Ollama. Requires a configured Ollama endpoint —
	// otherwise the model truly isn't reachable via /api/*.
	ollama, ok := s.backend.Resolve(constants.AppOllama)
	if !ok {
		return ollamaUnavailableErr(modelName)
	}
	recorder.SetProvider(constants.AppOllama)
	recorder.SetRouting(llm.RoutingDecisionLocal, s.node.Nodename())
	routing.RecordDecision(c.Request.Context(),
		s.routingStrategy(), routing.OutcomeLocal,
		string(genai.ProviderName(constants.AppOllama)), operation)
	target := backend.PassthroughTarget(ollama.Endpoint, c.Request.URL.Path, c.Request.URL.RawQuery)
	s.proxy.ForwardToBackend(c.Writer, c.Request, constants.AppOllama, target, ollama.Upstream, body)
	return nil
}

// ollamaUnavailableErr is the canonical 404 returned when a model is not
// reachable via the Ollama API surface. Kept as a helper so the message is
// identical across all callers.
func ollamaUnavailableErr(modelName string) *RoutedError {
	return newProblemError(http.StatusNotFound, "Not Found",
		fmt.Sprintf("model %q is not available via the Ollama API; use /v1/chat/completions", modelName))
}

// newOllamaInferenceRecorder builds and stashes an inference recorder for an
// Ollama-compat request. endpointType is the Ollama route name the
// request came in on ("chat", "generate", "embeddings", "embed"); it
// maps onto the OTel-canonical request type so /api/embed lands as
// operation_name=embeddings on /metrics rather than the previous
// (incorrect) operation_name=chat.
func newOllamaInferenceRecorder(c *gin.Context, model string, body []byte, endpointType string) *llm.InferenceRecorder {
	recorder := llm.NewInferenceRecorder(c.Request.Context(), model, "")
	recorder.SetRequestType(ollamaRequestType(endpointType))
	recorder.SetStream(ollamaStreamRequested(body, endpointType))
	recorder.SetRequestData(body)
	mirrorCallerIdentityToRecorder(c, recorder)
	ctx := context.WithValue(c.Request.Context(), CtxKeyInferenceRecorder, recorder)
	c.Request = c.Request.WithContext(ctx)
	return recorder
}

// ollamaStreamRequested reports whether the answer will be streamed.
//
// Ollama's default is the opposite of OpenAI's: /api/chat and /api/generate
// stream unless the caller opts out, so an absent field means true and a
// pointer is needed to tell absent from an explicit false. Embedding routes
// never stream.
//
// Nothing on this surface set the flag at all before, so every /api/*
// request was logged stream:false — including the ones that streamed. Any
// report splitting traffic by streaming read the Ollama surface as entirely
// non-streaming.
func ollamaStreamRequested(body []byte, endpointType string) bool {
	if ollamaRequestType(endpointType) == llm.RequestTypeEmbedding {
		return false
	}
	var probe struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Stream == nil {
		return true
	}
	return *probe.Stream
}

// ollamaRequestType maps an Ollama route name onto the canonical
// llm.RequestType vocabulary. Both /api/embeddings (legacy) and
// /api/embed (newer) map to the embedding type. Unknown / empty
// falls back to chat — matches prior behavior so misuse is visible
// (chat operation on a non-chat endpoint stands out in dashboards)
// rather than silently dropped.
func ollamaRequestType(endpointType string) string {
	switch endpointType {
	case "chat":
		return llm.RequestTypeChat
	case "generate":
		return llm.RequestTypeGenerate
	case "embeddings", "embed":
		return llm.RequestTypeEmbedding
	default:
		return llm.RequestTypeChat
	}
}
