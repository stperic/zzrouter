package llm

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.40.0/genaiconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/utils"
)

// Metrics holds all LLM-specific metrics instruments.
//
// The `gen_ai.*` instruments follow the OpenTelemetry GenAI semantic
// conventions (via genaiconv) and are the canonical source of inference
// observability data. The remaining `zzrouter.*` instruments cover concepts
// OTel does not yet define (model-load lifecycle, instance counters) and are
// kept under their existing names.
type Metrics struct {
	// Model loading metrics (no OTel canonical — kept on zzrouter.* names).
	modelLoadDuration metric.Float64Histogram
	modelLoadTotal    metric.Int64Counter

	// OTel GenAI inference metrics (typed semconv instruments).
	clientOperationDuration genaiconv.ClientOperationDuration
	clientTokenUsage        genaiconv.ClientTokenUsage
	serverTimeToFirstToken  genaiconv.ServerTimeToFirstToken
	serverRequestDuration   genaiconv.ServerRequestDuration
	serverTimePerOutputTok  genaiconv.ServerTimePerOutputToken

	// Instance metrics (no OTel canonical — kept on zzrouter.* names).
	instancesActive metric.Int64UpDownCounter
}

var (
	metricsMu      sync.Mutex
	globalMetrics  *Metrics
	globalProvider metric.MeterProvider
)

// InitMetrics initializes the LLM metrics against the current global OTel
// meter provider. Safe to call multiple times — if the global provider has
// changed since the last call (e.g. between tests that each install their own
// provider), the metrics are rebuilt against the new one so recordings land
// in the right exporter.
func InitMetrics() (*Metrics, error) {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	// Interface comparison: matches when the same concrete provider pointer is
	// reused. A wrapping/decorating provider will compare unequal and trigger a
	// rebuild, which is the safer default.
	current := otel.GetMeterProvider()
	if globalMetrics != nil && globalProvider == current {
		return globalMetrics, nil
	}
	m, err := newMetrics()
	if err != nil {
		return globalMetrics, err
	}
	globalMetrics = m
	globalProvider = current
	return globalMetrics, nil
}

// GetMetrics returns the global LLM metrics instance, initializing it (or
// rebuilding it against the current OTel meter provider) on demand.
func GetMetrics() *Metrics {
	m, _ := InitMetrics()
	return m
}

func newMetrics() (*Metrics, error) {
	meter := otel.Meter("zzrouter.llm")

	m := &Metrics{}
	var err error

	// Model loading metrics
	m.modelLoadDuration, err = meter.Float64Histogram(
		"zzrouter.model.load.duration",
		metric.WithDescription("Model loading time in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return nil, err
	}

	m.modelLoadTotal, err = meter.Int64Counter(
		"zzrouter.model.load.total",
		metric.WithDescription("Total model load attempts"),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return nil, err
	}

	// OTel GenAI instruments — constructed via genaiconv typed helpers.
	// Bucket boundaries are passed as options; the constructor sets the
	// correct name, unit, and description from the spec.
	m.clientOperationDuration, err = genaiconv.NewClientOperationDuration(meter,
		metric.WithExplicitBucketBoundaries(
			0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56,
			5.12, 10.24, 20.48, 40.96, 81.92,
		),
	)
	if err != nil {
		return nil, err
	}

	m.clientTokenUsage, err = genaiconv.NewClientTokenUsage(meter,
		metric.WithExplicitBucketBoundaries(
			1, 4, 16, 64, 256, 1024, 4096, 16384, 65536,
			262144, 1048576, 4194304, 16777216, 67108864,
		),
	)
	if err != nil {
		return nil, err
	}

	m.serverTimeToFirstToken, err = genaiconv.NewServerTimeToFirstToken(meter,
		metric.WithExplicitBucketBoundaries(
			0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1,
			0.25, 0.5, 0.75, 1.0, 2.5, 5.0, 7.5, 10.0,
		),
	)
	if err != nil {
		return nil, err
	}

	// gen_ai.server.request.duration: server-side perceived latency.
	// For zzrouter (a routing layer) this is observed at the same
	// timestamps as client.operation.duration — the distinction in the
	// spec separates client-perceived (network round-trip included)
	// from server-perceived (server-internal only). We emit on both
	// names so downstream consumers (LiteLLM-style dashboards, OTLP
	// collectors filtering by view) can pick whichever fits their
	// model. Bucket boundaries match operation duration.
	m.serverRequestDuration, err = genaiconv.NewServerRequestDuration(meter,
		metric.WithExplicitBucketBoundaries(
			0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56,
			5.12, 10.24, 20.48, 40.96, 81.92,
		),
	)
	if err != nil {
		return nil, err
	}

	// gen_ai.server.time_per_output_token: post-TTFT throughput
	// expressed as seconds-per-token. Streaming-only and meaningful
	// only when output_tokens >= noiseFloorTokensOut. Bucket boundaries
	// span the typical range for hosted models (a few ms/tok on
	// fast GPU through hundreds of ms/tok for large models on CPU).
	m.serverTimePerOutputTok, err = genaiconv.NewServerTimePerOutputToken(meter,
		metric.WithExplicitBucketBoundaries(
			0.001, 0.0025, 0.005, 0.0075, 0.01, 0.025, 0.05, 0.075, 0.1,
			0.25, 0.5, 0.75, 1.0, 2.5,
		),
	)
	if err != nil {
		return nil, err
	}

	// Instance metrics
	m.instancesActive, err = meter.Int64UpDownCounter(
		"zzrouter.instances.active",
		metric.WithDescription("Number of active LLM instances"),
		metric.WithUnit("{instance}"),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// RecordModelLoad records a model load attempt.
//
// model is caller-controlled (the request body's model field for
// /runs/load and similar paths) and is gated through ModelName, which
// applies genai.GateModelLabel. Without the gate, an adversarial
// caller could explode the model_load metric's model dimension.
func (m *Metrics) RecordModelLoad(ctx context.Context, model, provider, status string, durationSeconds float64) {
	if m == nil {
		return
	}

	attrs := []attribute.KeyValue{
		ModelName(model),
		ModelProvider(provider),
		attribute.String("status", status),
	}

	m.modelLoadTotal.Add(ctx, 1, metric.WithAttributes(attrs...))

	if status == "success" {
		m.modelLoadDuration.Record(ctx, durationSeconds, metric.WithAttributes(
			ModelName(model),
			ModelProvider(provider),
		))
	}
}

// optionalAttrs builds the variadic attribute.KeyValue slice for the
// optional/conditionally-required OTel GenAI attributes. Uses the typed
// attribute helpers from genaiconv to ensure correct types.
//
// requestModel + responseModel are caller-controlled values that pass
// through genai.GateModelLabel before landing on a label — when the
// operator has wired SetModelAllowlist, anything not on the allowlist
// collapses to UnknownModelLabel rather than emitting the raw value
// as a separate Prometheus series. Without the gate, the success path
// (the dominant volume of inference traffic) would be wide open to
// adversarial cardinality spraying.
func optionalAttrs(inst genaiconv.ClientOperationDuration, requestModel, responseModel, errorType, serverAddress string, serverPort int) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	if gated := genai.GateModelLabel(requestModel); gated != "" {
		attrs = append(attrs, inst.AttrRequestModel(gated))
	}
	if gated := genai.GateModelLabel(responseModel); gated != "" {
		attrs = append(attrs, inst.AttrResponseModel(gated))
	}
	if errorType != "" {
		attrs = append(attrs, inst.AttrErrorType(genaiconv.ErrorTypeAttr(errorType)))
	}
	if serverAddress != "" {
		attrs = append(attrs, inst.AttrServerAddress(serverAddress))
		if serverPort > 0 {
			attrs = append(attrs, inst.AttrServerPort(serverPort))
		}
	}
	return attrs
}

// RecordGenAIClientCall records the OTel GenAI client metrics for a single
// inference call: `gen_ai.client.operation.duration` plus
// `gen_ai.client.token.usage` (one observation per token type that was
// non-zero, so embeddings or speech routes that produce no output tokens
// don't pollute the output histogram with zeros).
func (m *Metrics) RecordGenAIClientCall(
	ctx context.Context,
	operation genaiconv.OperationNameAttr,
	provider genaiconv.ProviderNameAttr,
	requestModel, responseModel, errorType, serverAddress string,
	serverPort int,
	durationSec float64,
	tokensIn, tokensOut int64,
) {
	if m == nil {
		return
	}
	attrs := optionalAttrs(m.clientOperationDuration, requestModel, responseModel, errorType, serverAddress, serverPort)

	m.clientOperationDuration.Record(ctx, durationSec, operation, provider, attrs...)

	if tokensIn > 0 {
		m.clientTokenUsage.Record(ctx, tokensIn, operation, provider, genaiconv.TokenTypeInput, attrs...)
	}
	if tokensOut > 0 {
		m.clientTokenUsage.Record(ctx, tokensOut, operation, provider, genaiconv.TokenTypeOutput, attrs...)
	}
}

// RecordGenAIServerTimeToFirstToken records the OTel GenAI
// `gen_ai.server.time_to_first_token` histogram for a streaming response.
func (m *Metrics) RecordGenAIServerTimeToFirstToken(
	ctx context.Context,
	operation genaiconv.OperationNameAttr,
	provider genaiconv.ProviderNameAttr,
	requestModel, responseModel, serverAddress string,
	serverPort int,
	ttftSec float64,
) {
	if m == nil {
		return
	}
	attrs := optionalAttrs(m.clientOperationDuration, requestModel, responseModel, "", serverAddress, serverPort)
	m.serverTimeToFirstToken.Record(ctx, ttftSec, operation, provider, attrs...)
}

// RecordGenAIServerRequestDuration records the OTel GenAI
// `gen_ai.server.request.duration` histogram. Always emits, on both
// success and error paths — server-perceived latency reflects time
// spent routing + dispatching even when the upstream fails.
func (m *Metrics) RecordGenAIServerRequestDuration(
	ctx context.Context,
	operation genaiconv.OperationNameAttr,
	provider genaiconv.ProviderNameAttr,
	requestModel, responseModel, errorType, serverAddress string,
	serverPort int,
	durationSec float64,
) {
	if m == nil {
		return
	}
	attrs := optionalAttrs(m.clientOperationDuration, requestModel, responseModel, errorType, serverAddress, serverPort)
	m.serverRequestDuration.Record(ctx, durationSec, operation, provider, attrs...)
}

// RecordGenAIServerTimePerOutputToken records the OTel GenAI
// `gen_ai.server.time_per_output_token` histogram (sec/token,
// post-TTFT). Streaming-only — non-streaming has no TTFT, making
// the post-TTFT-throughput interpretation meaningless. Caller must
// pre-gate: only call when ttftSec > 0 AND output_tokens is above
// the noise floor.
func (m *Metrics) RecordGenAIServerTimePerOutputToken(
	ctx context.Context,
	operation genaiconv.OperationNameAttr,
	provider genaiconv.ProviderNameAttr,
	requestModel, responseModel, serverAddress string,
	serverPort int,
	tpoTokSec float64,
) {
	if m == nil {
		return
	}
	attrs := optionalAttrs(m.clientOperationDuration, requestModel, responseModel, "", serverAddress, serverPort)
	m.serverTimePerOutputTok.Record(ctx, tpoTokSec, operation, provider, attrs...)
}

// RecordInstanceStart records an instance starting.
func (m *Metrics) RecordInstanceStart(ctx context.Context, model, provider string) {
	if m == nil {
		return
	}

	m.instancesActive.Add(ctx, 1, metric.WithAttributes(
		ModelName(model),
		ModelProvider(provider),
	))
}

// RecordInstanceStop records an instance stopping.
func (m *Metrics) RecordInstanceStop(ctx context.Context, model, provider string) {
	if m == nil {
		return
	}

	m.instancesActive.Add(ctx, -1, metric.WithAttributes(
		ModelName(model),
		ModelProvider(provider),
	))
}

// RequestTypeToOperation translates the zzrouter request-type vocabulary
// (chat/completion/embedding/generate) onto OTel GenAI operation values.
func RequestTypeToOperation(rt string) genaiconv.OperationNameAttr {
	switch rt {
	case RequestTypeChat, RequestTypeGenerate:
		return genai.OperationChat
	case RequestTypeCompletion:
		return genai.OperationTextCompletion
	case RequestTypeEmbedding:
		return genai.OperationEmbeddings
	default:
		return ""
	}
}

// InferenceRecorder is a helper for recording inference metrics.
// It tracks the start time and can record metrics when the inference completes.
// It also collects data for the inference log hook when one is registered.
//
// InferenceRecorder is not safe for concurrent use. Callers must ensure that
// all Set* methods complete before RecordFirstToken or RecordCompletion is
// called. In practice the handler goroutine owns the recorder through the
// enrichment phase, then hands it to the streaming path which calls
// RecordFirstToken and RecordCompletion sequentially.
type InferenceRecorder struct {
	ctx      context.Context
	metrics  *Metrics
	model    string
	provider string
	startNs  int64

	// OTel GenAI fields. operation overrides the value derived from
	// requestType; responseModel/serverAddress/serverPort are optional.
	// providerAttr caches the genaiconv.ProviderNameAttr so the mapping
	// is computed once (at SetProvider/construction) rather than per-record.
	operation    genaiconv.OperationNameAttr
	providerAttr genaiconv.ProviderNameAttr

	responseModel string
	serverAddress string
	serverPort    int

	// Inference log fields (populated by handler enrichment)
	requestType     string
	stream          bool
	routingDecision string
	node            string
	ttftNs          int64
	status          string
	errorType       string
	errorMessage    string
	requestBody     []byte
	upstreamBody    []byte
	responseBody    []byte
	responseText    strings.Builder
	groupName       string
	deploymentName  string
	fallbackCount   int
	fallbackFrom    string
	keyID           string
	keyAlias        string
	hashedKey       string
	teamID          string
	teamAlias       string
	reservationID   uint64

	// Extended usage fields (from cloud providers)
	tokensCached    int64
	tokensReasoning int64
	tokensOut       int64 // Mirror of RecordCompletion's tokensOut for Snapshot.
	cost            float64
	costSource      string // "" | "provider" | "zzrouter"
}

// noiseFloorTokensOut suppresses tokens_per_second when fewer than this
// many output tokens were produced — a 1- or 2-token completion is
// statistically meaningless throughput.
const noiseFloorTokensOut = 5

// RequestSnapshot is a value-typed view of the recorder's current
// per-request state, used by the wire layer to stamp cost+timing
// fields onto response bodies. Returned by (*InferenceRecorder).Snapshot.
type RequestSnapshot struct {
	LatencyMs    int64
	TTFTMs       int64
	TokensOut    int64
	Cost         float64
	CostSource   string
	TokensPerSec float64
}

// NewInferenceRecorder creates a new inference recorder.
func NewInferenceRecorder(ctx context.Context, model, provider string) *InferenceRecorder {
	return &InferenceRecorder{
		ctx:          ctx,
		metrics:      GetMetrics(),
		model:        model,
		provider:     provider,
		providerAttr: genai.ProviderName(provider),
		startNs:      nanotime(),
	}
}

// SetRequestType sets the request type (chat, completion, embedding, generate).
func (r *InferenceRecorder) SetRequestType(rt string) {
	if r != nil {
		r.requestType = rt
	}
}

// SetOperation overrides the OTel GenAI `gen_ai.operation.name` value.
// Use the typed constants in `pkg/observability/genai`. When unset, the
// operation is derived from the request type via requestTypeToOperation.
func (r *InferenceRecorder) SetOperation(op genaiconv.OperationNameAttr) {
	if r != nil {
		r.operation = op
	}
}

// SetResponseModel sets the OTel `gen_ai.response.model` attribute — the model
// the upstream backend actually used. Important when model-group routing
// resolves a group name to a concrete deployment.
func (r *InferenceRecorder) SetResponseModel(m string) {
	if r != nil {
		r.responseModel = m
	}
}

// SetServer sets the OTel `server.address` / `server.port` attributes for the
// upstream backend.
func (r *InferenceRecorder) SetServer(address string, port int) {
	if r != nil {
		r.serverAddress = address
		r.serverPort = port
	}
}

// SetStream sets whether the request is streaming.
func (r *InferenceRecorder) SetStream(streaming bool) {
	if r != nil {
		r.stream = streaming
	}
}

// SetRouting sets the routing decision and target node.
func (r *InferenceRecorder) SetRouting(decision, node string) {
	if r != nil {
		r.routingDecision = decision
		r.node = node
	}
}

// SetError sets error information for the inference.
func (r *InferenceRecorder) SetError(errType, message string) {
	if r != nil {
		r.status = "error"
		r.errorType = errType
		r.errorMessage = message
	}
}

// SetRequestData stores the raw request body for prompt extraction by the log bridge.
func (r *InferenceRecorder) SetRequestData(body []byte) {
	if r != nil {
		r.requestBody = body
	}
}

// SetUpstreamRequestData stores the body as it goes on the wire to the
// provider, which differs from the client body whenever routing rewrites
// the model name. Last write wins, so a fallback chain records the
// deployment that actually served the request.
func (r *InferenceRecorder) SetUpstreamRequestData(body []byte) {
	if r != nil {
		r.upstreamBody = body
	}
}

// SetResponseBody stores a complete non-streaming reply body. Streaming
// replies have no single body — they accumulate through AppendResponseText.
func (r *InferenceRecorder) SetResponseBody(body []byte) {
	if r != nil && CaptureResponses() {
		r.responseBody = body
	}
}

// AppendResponseText accumulates the assistant's reply. Streaming callers
// append per chunk; non-streaming callers append once. Accumulation stops
// at maxResponseTextBytes so one runaway generation cannot pin memory in
// the log ring.
func (r *InferenceRecorder) AppendResponseText(s string) {
	if r == nil || s == "" || !CaptureResponses() {
		return
	}
	if remaining := maxResponseTextBytes - r.responseText.Len(); remaining > 0 {
		if len(s) > remaining {
			s = s[:remaining]
		}
		r.responseText.WriteString(s)
	}
}

// SetExtendedUsage sets additional usage fields from cloud provider responses.
// source is the closed-enum tag identifying where cost came from (e.g.
// "provider" when upstream returned usage.cost, "" when the value is
// not yet authoritative — the bridge populates "zzrouter" downstream
// after pricing-store compute).
func (r *InferenceRecorder) SetExtendedUsage(cached, reasoning int64, cost float64, source string) {
	if r != nil {
		r.tokensCached = cached
		r.tokensReasoning = reasoning
		r.cost = cost
		r.costSource = source
	}
}

// SetCost overwrites the cost number and source on the recorder.
// Used by the inference-log bridge after pricing-store fallback so the
// response inject path (running before the bridge) and the ledger
// settle (running inside the bridge) agree on the final number.
func (r *InferenceRecorder) SetCost(cost float64, source string) {
	if r != nil {
		r.cost = cost
		r.costSource = source
	}
}

// SetTokensOut stashes the output-token count on the recorder so the
// streaming inject path (which runs before RecordCompletion) can read
// the value via Snapshot. Called at parse time on each terminal usage
// frame in copy.go; idempotent for non-streaming since the same value
// flows through RecordCompletion afterward.
func (r *InferenceRecorder) SetTokensOut(out int64) {
	if r != nil {
		r.tokensOut = out
	}
}

// Snapshot returns a value-typed view of the recorder's current
// per-request state. Safe to call mid-request; values reflect whatever
// has been populated up to that point.
//
// TokensPerSec is computed as post-TTFT throughput when ttftNs is set
// (the conventional streaming metric), otherwise falls back to full
// duration for non-streaming. Values below the noise floor
// (noiseFloorTokensOut) return TokensPerSec=0 — a one- or two-token
// completion has no meaningful throughput.
func (r *InferenceRecorder) Snapshot() RequestSnapshot {
	if r == nil {
		return RequestSnapshot{}
	}
	// ttftNs is stored as a DURATION (nanotime() - startNs at the
	// first-token moment) by RecordFirstToken; the bridge already
	// reads it that way. Treat it as duration here too.
	now := nanotime()
	var latencyMs, ttftMs int64
	if r.startNs > 0 {
		latencyMs = (now - r.startNs) / 1e6
	}
	if r.ttftNs > 0 {
		ttftMs = r.ttftNs / 1e6
	}

	var tps float64
	if r.tokensOut >= noiseFloorTokensOut && r.startNs > 0 {
		// Post-TTFT elapsed = total elapsed - TTFT. Falls back to full
		// duration when ttftNs unset (non-streaming).
		elapsedNs := now - r.startNs - r.ttftNs
		if elapsedNs > 0 {
			tps = float64(r.tokensOut) / (float64(elapsedNs) / 1e9)
		}
	}

	return RequestSnapshot{
		LatencyMs:    latencyMs,
		TTFTMs:       ttftMs,
		TokensOut:    r.tokensOut,
		Cost:         r.cost,
		CostSource:   r.costSource,
		TokensPerSec: tps,
	}
}

// SetModelGroup sets the model group and deployment name for group-resolved models.
func (r *InferenceRecorder) SetModelGroup(groupName, deploymentName string) {
	if r != nil {
		r.groupName = groupName
		r.deploymentName = deploymentName
	}
}

// SetFallback sets the fallback routing info (how many deployments were tried, and which was first).
func (r *InferenceRecorder) SetFallback(count int, firstDeployment string) {
	if r != nil {
		r.fallbackCount = count
		r.fallbackFrom = firstDeployment
	}
}

// SetKeyID sets the virtual key ID for post-response tracking.
func (r *InferenceRecorder) SetKeyID(id string) {
	if r != nil {
		r.keyID = id
	}
}

// SetTeamID sets the team ID the calling key belongs to, for post-response
// attribution. Empty string = key has no team (teamless keys are allowed).
func (r *InferenceRecorder) SetTeamID(id string) {
	if r != nil {
		r.teamID = id
	}
}

// SetCallerIdentity stashes the LiteLLM-vocabulary caller labels
// (api_key_alias, hashed_api_key, team_alias) on the recorder. The
// dispatcher reads these from gin.Context (the proxy package's
// CtxKeyAPIKeyAlias / CtxKeyHashedAPIKey / CtxKeyTeamAlias keys, set by
// access_context.setAccessContext) and forwards them here so the
// post-response inference-log bridge can emit zz.input.tokens.metric /
// zz.output.tokens.metric / zz.spend.metric.total without holding a
// reference to the gin context. Empty values are valid for
// unauthenticated or static-key callers.
func (r *InferenceRecorder) SetCallerIdentity(keyAlias, hashedKey, teamAlias string) {
	if r != nil {
		r.keyAlias = keyAlias
		r.hashedKey = hashedKey
		r.teamAlias = teamAlias
	}
}

// SetReservationID stores the budget-reservation stash ID returned by
// AccessControl.Enforce. Threaded through to the inference log hook
// so the bridge can settle the exact reservation this request
// created, avoiding FIFO cross-settlement against concurrent same-key
// requests. Zero is the "no reservation held" sentinel.
func (r *InferenceRecorder) SetReservationID(id uint64) {
	if r != nil {
		r.reservationID = id
	}
}

// SetModel updates the model name (useful when model is resolved after creation).
func (r *InferenceRecorder) SetModel(model string) {
	if r != nil {
		r.model = model
	}
}

// SetProvider updates the provider name and caches the OTel provider attr.
func (r *InferenceRecorder) SetProvider(provider string) {
	if r != nil {
		r.provider = provider
		r.providerAttr = genai.ProviderName(provider)
	}
}

// operationName returns the OTel operation value for this recorder, falling
// back to a translation of the request-type field.
func (r *InferenceRecorder) operationName() genaiconv.OperationNameAttr {
	if r.operation != "" {
		return r.operation
	}
	return RequestTypeToOperation(r.requestType)
}

// RecordCompletion records the inference completion.
func (r *InferenceRecorder) RecordCompletion(tokensIn, tokensOut int64) {
	if r == nil || r.metrics == nil {
		return
	}

	// Stash tokensOut so any post-completion Snapshot reader (the
	// non-streaming response inject runs after this returns) sees the
	// value for tokens_per_second.
	r.tokensOut = tokensOut

	// Enrich the active inference span with the same conditionally-required
	// + recommended OTel GenAI attributes the metrics carry. Funnels every
	// error path too: SetError + RecordCompletion(0,0) is the canonical
	// pre-byte-copy abort shape, so a single hook covers both success and
	// failure cases.
	r.enrichSpan(tokensIn, tokensOut)

	durationNs := nanotime() - r.startNs
	durationSeconds := float64(durationNs) / 1e9

	r.metrics.RecordGenAIClientCall(
		r.ctx,
		r.operationName(),
		r.providerAttr,
		r.model,
		r.responseModel,
		r.errorType,
		r.serverAddress,
		r.serverPort,
		durationSeconds,
		tokensIn,
		tokensOut,
	)

	// gen_ai.server.request.duration mirrors the client view for
	// zzrouter (we observe the same timestamps). Always emits,
	// including error paths — error_type tags the series so dashboards
	// can split error rates server-side too.
	r.metrics.RecordGenAIServerRequestDuration(
		r.ctx,
		r.operationName(),
		r.providerAttr,
		r.model,
		r.responseModel,
		r.errorType,
		r.serverAddress,
		r.serverPort,
		durationSeconds,
	)

	// gen_ai.server.time_per_output_token: only meaningful for
	// streaming completions with enough tokens to give a stable
	// per-token average. Non-streaming has ttftNs=0, which would
	// degenerate to total-duration/tokens — fine arithmetic, wrong
	// semantic — so we skip those.
	if r.ttftNs > 0 && tokensOut >= noiseFloorTokensOut {
		postTTFTSec := durationSeconds - float64(r.ttftNs)/1e9
		if postTTFTSec > 0 {
			tpoTokSec := postTTFTSec / float64(tokensOut)
			r.metrics.RecordGenAIServerTimePerOutputToken(
				r.ctx,
				r.operationName(),
				r.providerAttr,
				r.model,
				r.responseModel,
				r.serverAddress,
				r.serverPort,
				tpoTokSec,
			)
		}
	}

	// Fire inference log hook if registered
	if hook := getInferenceLogHook(); hook != nil {
		status := r.status
		if status == "" {
			status = "success"
		}
		hook.OnInferenceComplete(InferenceLogData{
			CostSetter:      r,
			Model:           r.model,
			ResponseModel:   r.responseModel,
			App:             r.provider,
			RequestType:     r.requestType,
			Stream:          r.stream,
			RoutingDecision: r.routingDecision,
			Node:            r.node,
			TokensIn:        tokensIn,
			TokensOut:       tokensOut,
			TokensCached:    r.tokensCached,
			TokensReasoning: r.tokensReasoning,
			Cost:            r.cost,
			CostSource:      r.costSource,
			LatencyNs:       durationNs,
			TTFTNs:          r.ttftNs,
			Status:          status,
			GroupName:       r.groupName,
			DeploymentName:  r.deploymentName,
			FallbackCount:   r.fallbackCount,
			FallbackFrom:    r.fallbackFrom,
			KeyID:           r.keyID,
			KeyAlias:        r.keyAlias,
			HashedKey:       r.hashedKey,
			TeamID:          r.teamID,
			TeamAlias:       r.teamAlias,
			ReservationID:   r.reservationID,
			ErrorType:       r.errorType,
			ErrorMessage:    r.errorMessage,
			RequestBody:     r.requestBody,
			UpstreamBody:    r.upstreamBody,
			ResponseBody:    r.responseBody,
			ResponseText:    r.responseText.String(),
		})
	}
}

// enrichSpan stamps the OTel GenAI conditionally-required + recommended
// attributes on the active inference span. Mirrors the metric attributes
// so dashboards and trace viewers slice the same values. No-op when the
// recorder's context carries a noop span.
func (r *InferenceRecorder) enrichSpan(tokensIn, tokensOut int64) {
	span := trace.SpanFromContext(r.ctx)
	if !span.IsRecording() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	if tokensIn > 0 {
		attrs = append(attrs, UsageInputTokens(tokensIn))
	}
	if tokensOut > 0 {
		attrs = append(attrs, UsageOutputTokens(tokensOut))
	}
	if r.responseModel != "" {
		attrs = append(attrs, ResponseModel(r.responseModel))
	}
	if r.serverAddress != "" {
		attrs = append(attrs, ServerAddress(r.serverAddress))
		if r.serverPort > 0 {
			attrs = append(attrs, ServerPort(r.serverPort))
		}
	}
	if r.providerAttr != "" {
		// providerAttr is already the canonical OTel value (set at
		// SetProvider via genai.ProviderName). Skip the round-trip
		// through ModelProvider's mapping helper.
		attrs = append(attrs, ModelProviderKey.String(string(r.providerAttr)))
	}
	if r.errorType != "" {
		attrs = append(attrs, ErrorType(r.errorType))
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

// RecordFirstToken records time to first token.
func (r *InferenceRecorder) RecordFirstToken() {
	if r == nil || r.metrics == nil {
		return
	}

	ttftNs := nanotime() - r.startNs
	r.ttftNs = ttftNs // Store for log hook
	ttftSeconds := float64(ttftNs) / 1e9

	r.metrics.RecordGenAIServerTimeToFirstToken(
		r.ctx,
		r.operationName(),
		r.providerAttr,
		r.model,
		r.responseModel,
		r.serverAddress,
		r.serverPort,
		ttftSeconds,
	)
}

// nanotime returns the current time in nanoseconds.
func nanotime() int64 {
	return utils.Now().UnixNano()
}
