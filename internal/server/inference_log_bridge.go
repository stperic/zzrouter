// package server provides HTTP handlers for the zzrouter host server.
// InferenceLogBridge adapts the observability hook to the inferencelog store.
// Post-response spend settlement is routed through AccessControl.

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/stperic/zzrouter/pkg/access/quota"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/inferencelog"
	"github.com/stperic/zzrouter/pkg/jobs"
	"github.com/stperic/zzrouter/pkg/model/pricing"
	"github.com/stperic/zzrouter/pkg/observability/genai"
	"github.com/stperic/zzrouter/pkg/observability/llm"
	"github.com/stperic/zzrouter/pkg/observability/spend"
	"github.com/stperic/zzrouter/pkg/utils"
)

// InferenceLogBridge implements llm.InferenceLogHook and writes entries
// to the inference log store. It also settles budget reservations and
// records TPM tokens via the AccessControl. When a jobs.Handle is
// attached (see SetJobHandle), every completed inference also mirrors
// onto the firehose SSE stream.
type InferenceLogBridge struct {
	store          *inferencelog.Store
	nodeName       string
	capturePrompts bool
	pricingStore   *pricing.Store
	access         *AccessControl // nil-safe — set post-init via SetAccessControl
	latencyTracker *fallback.LatencyTracker

	// jobHandle, when non-nil, is a long-lived KindInferenceLog handle
	// that mirrors every log entry onto the shared jobs stream.
	// Subscribers of /zzrouter/v1/jobs/:id/stream receive each entry as
	// a running-phase event with the entry payload in Meta["entry"].
	// Firehose kind: no ring, no replay.
	jobHandle jobs.Handle
}

// NewInferenceLogBridge creates a bridge between the OTel inference recorder and the log store.
func NewInferenceLogBridge(store *inferencelog.Store, nodeName string, capturePrompts bool, pricingStore *pricing.Store) *InferenceLogBridge {
	return &InferenceLogBridge{
		store:          store,
		nodeName:       nodeName,
		capturePrompts: capturePrompts,
		pricingStore:   pricingStore,
	}
}

// SetAccessControl wires the AccessControl for post-response spend settlement.
// Called after the gateway is constructed.
func (b *InferenceLogBridge) SetAccessControl(g *AccessControl) {
	b.access = g
}

// SetLatencyTracker sets the latency tracker for feeding the fastest routing strategy.
func (b *InferenceLogBridge) SetLatencyTracker(lt *fallback.LatencyTracker) {
	b.latencyTracker = lt
}

// SetJobHandle attaches a long-lived jobs.Handle to the bridge. Called
// at server startup with a KindInferenceLog handle; subsequent log
// entries are mirrored onto the SSE stream.
func (b *InferenceLogBridge) SetJobHandle(h jobs.Handle) {
	b.jobHandle = h
}

// OnInferenceComplete is called by InferenceRecorder.RecordCompletion.
func (b *InferenceLogBridge) OnInferenceComplete(data llm.InferenceLogData) {
	latencyMs := float64(data.LatencyNs) / 1e6
	ttftMs := float64(data.TTFTNs) / 1e6

	var tokensPerSec float64
	if latencyMs > 0 && data.TokensOut > 0 {
		tokensPerSec = float64(data.TokensOut) / (latencyMs / 1000)
	}

	node := data.Node
	if node == "" {
		node = b.nodeName
	}

	// Resolve cost + source: provider-authoritative if upstream gave
	// us one; otherwise fall back to pricing-store compute. The source
	// tag flows onto the LogEntry for audit + onto the spend ledger
	// indirectly (CostSource isn't on the ledger today; deferred).
	// ResponseModel leads: it is the id the upstream reported serving,
	// which is the only one a price table can know when model-group
	// routing resolved a local alias to this deployment.
	costMicro, costSource := quota.CalculateCostMicro(data.Cost, b.pricingStore, data.App,
		[]string{data.ResponseModel, data.Model},
		data.TokensIn, data.TokensOut, data.TokensCached, data.TokensReasoning)
	costUSD := data.Cost
	if costSource == quota.CostSourceZZRouter {
		costUSD = quota.MicroToUSD(costMicro)
	}
	// No source on a request that actually moved tokens means nothing
	// priced it: it settles at $0 and draws down no budget. Record it
	// so the gap is visible on /pricing/status instead of looking free.
	if costSource == "" && (data.TokensIn > 0 || data.TokensOut > 0) {
		b.pricingStore.RecordMiss(data.App, data.ResponseModel, data.Model)
	}
	// Push the resolved (cost, source) back onto the recorder so any
	// post-completion Snapshot reader observes the pricing-store-
	// upgraded value instead of the raw upstream-only one.
	//   - Non-streaming: the response inject runs after this hook
	//     fires, so the body picks up the upgrade directly.
	//   - Streaming: the per-chunk transforms already ran with
	//     whatever the upstream gave them; the synthetic-final-chunk
	//     path (commit.go's no-usage fallback) runs AFTER this and
	//     does pick up the upgrade for that surface.
	if data.CostSetter != nil {
		data.CostSetter.SetCost(costUSD, costSource)
	}

	entry := inferencelog.LogEntry{
		ID:              generateID(),
		Timestamp:       utils.Now(),
		Model:           data.Model,
		ResponseModel:   data.ResponseModel,
		App:             data.App,
		RequestType:     data.RequestType,
		Stream:          data.Stream,
		RoutingDecision: data.RoutingDecision,
		Node:            node,
		Status:          data.Status,
		TokensIn:        data.TokensIn,
		TokensOut:       data.TokensOut,
		TokensCached:    data.TokensCached,
		TokensReasoning: data.TokensReasoning,
		TokensPerSec:    tokensPerSec,
		TTFTMs:          ttftMs,
		LatencyMs:       latencyMs,
		Cost:            costUSD,
		CostSource:      costSource,
		GroupName:       data.GroupName,
		DeploymentName:  data.DeploymentName,
		FallbackCount:   data.FallbackCount,
		FallbackFrom:    data.FallbackFrom,
		KeyID:           data.KeyID,
		TeamID:          data.TeamID,
		ErrorType:       data.ErrorType,
		ErrorMessage:    data.ErrorMessage,
	}

	// Parse request body for prompt details if capture is enabled
	if b.capturePrompts && len(data.RequestBody) > 0 {
		b.extractRequestDetails(&entry, data.RequestBody)
	}

	var payload *inferencelog.Payload
	if b.capturePrompts {
		entry.Response = data.ResponseText
		payload = inferencelog.NewPayload(data.RequestBody, data.UpstreamBody, data.ResponseBody)
	}
	entry.PayloadAvailable = b.store.Add(entry, payload)

	// Mirror onto the jobs firehose. Meta-then-Progress leaves each
	// emitted event carrying THIS entry in Meta["entry"] as a full
	// LogEntry (JSON tags on the struct drive the wire shape, so SSE
	// subscribers can decode straight back into inferencelog.LogEntry).
	// The firehose kind has no ring so merged-meta drift across events
	// is fine. Step = model for grep-ability in structured logs.
	if b.jobHandle != nil {
		b.jobHandle.Meta(jobs.Meta{"entry": entry})
		b.jobHandle.Progress(0, entry.Model, jobs.Bytes{})
	}

	// Feed latency data to the fastest routing strategy
	if b.latencyTracker != nil && data.DeploymentName != "" && data.LatencyNs > 0 {
		b.latencyTracker.Record(data.DeploymentName, time.Duration(data.LatencyNs))
	}

	// Post-response: settle budget reservation and record TPM tokens for virtual keys.
	if data.KeyID != "" && b.access != nil {
		b.access.RecordSpendByKey(data.KeyID, data.ReservationID, costMicro, data.TokensIn, data.TokensOut)
	}

	// LiteLLM-mirrored token + spend counters (zz.input.tokens.metric /
	// zz.output.tokens.metric / zz.spend.metric.total) — the LiteLLM
	// dashboard panels "tokens by key by day" and "spend by key by day"
	// filter on these. Counter view; orthogonal to the
	// gen_ai.client.token.usage histogram (which answers "p95 prompt
	// size") and the OTel spend has no canonical equivalent.
	model := data.DeploymentName
	if model == "" {
		model = data.Model
	}
	labels := spend.CallerLabels{
		APIKeyAlias:    data.KeyAlias,
		HashedAPIKey:   data.HashedKey,
		Team:           data.TeamID,
		TeamAlias:      data.TeamAlias,
		Model:          model,
		ModelGroup:     data.GroupName,
		RequestedModel: data.Model,
		APIProvider:    string(genai.ProviderName(data.App)),
	}
	spend.RecordTokens(context.Background(), labels, data.TokensIn, data.TokensOut)
	spend.RecordSpendUSD(context.Background(), labels, costUSD)
}

// extractRequestDetails parses the raw request JSON to populate prompt fields.
func (b *InferenceLogBridge) extractRequestDetails(entry *inferencelog.LogEntry, body []byte) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Prompt      string   `json:"prompt"`
		System      string   `json:"system"`
		Temperature *float64 `json:"temperature"`
		MaxTokens   *int     `json:"max_tokens"`
		TopP        *float64 `json:"top_p"`
		// Ollama-specific fields
		Options struct {
			Temperature *float64 `json:"temperature"`
			TopP        *float64 `json:"top_p"`
			NumPredict  *int     `json:"num_predict"`
		} `json:"options"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		return
	}

	entry.Temperature = req.Temperature
	entry.MaxTokens = req.MaxTokens
	entry.TopP = req.TopP

	// Ollama options fallback
	if entry.Temperature == nil && req.Options.Temperature != nil {
		entry.Temperature = req.Options.Temperature
	}
	if entry.TopP == nil && req.Options.TopP != nil {
		entry.TopP = req.Options.TopP
	}
	if entry.MaxTokens == nil && req.Options.NumPredict != nil {
		entry.MaxTokens = req.Options.NumPredict
	}

	// System prompt from Ollama-style field
	if req.System != "" {
		entry.SystemPrompt = req.System
	}

	// OpenAI-style messages
	if len(req.Messages) > 0 {
		for _, msg := range req.Messages {
			if msg.Role == "system" && entry.SystemPrompt == "" {
				entry.SystemPrompt = msg.Content
			}
			entry.Messages = append(entry.Messages, inferencelog.Message{
				Role:    msg.Role,
				Content: msg.Content,
			})
		}
	}

	// Ollama generate-style prompt
	if req.Prompt != "" && len(entry.Messages) == 0 {
		entry.Messages = []inferencelog.Message{
			{Role: "user", Content: req.Prompt},
		}
	}
}

// generateID creates a short random hex ID for log entries.
func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
