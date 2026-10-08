package wire

import (
	"bytes"
	"encoding/json"
	"strings"
)

// CostSource is a closed-enum tag describing where the cost number on
// the wire came from. Empty string means the value was not available
// (omit cost fields from the wire entirely).
type CostSource string

const (
	// CostSourceProvider — upstream returned an authoritative cost in
	// usage.cost (BYOK, prompt caching, surge pricing all baked in).
	CostSourceProvider CostSource = "provider"
	// CostSourceZZRouter — zzrouter computed cost via pricing-store
	// lookup × token counts.
	CostSourceZZRouter CostSource = "zzrouter"
)

// SuppressBodyFromVerbose maps a raw `?verbose=` query string value to
// the SuppressBodyMetadata flag. "0" → true (suppress); anything else
// (absent, "1", "true", typo) → false. Lenient on invalid values so an
// agent typo doesn't break inference; the safe default is to emit
// metadata as if no opt-out had been requested. Strict 400-on-invalid
// is an opt-in tightening for a later arc.
func SuppressBodyFromVerbose(raw string) bool {
	return raw == "0"
}

// FallbackAttempt records one dispatch try in the per-request fallback
// chain. Stamped on every response so agents can see which replicas
// were tried, in order, and what each returned.
type FallbackAttempt struct {
	Replica    string `json:"replica"`
	Status     int    `json:"status,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
}

// SkippedReplica records a replica that was not even tried — typically
// because it was in cooldown, unhealthy, or filtered out by per-call
// steering (Phase 3 / Tier 0.5). Reason is a closed-enum tag.
type SkippedReplica struct {
	Replica string `json:"replica"`
	Reason  string `json:"reason"`
}

// RoutingMetadata carries the values that the response inject path
// stamps onto a body or SSE chunk. Every cost/timing field is
// optional: a zero CostSource means the cost block is omitted from
// the wire, ttftMs == 0 means streaming-only field is suppressed,
// tokensPerSec == 0 means the noise-floor gate suppressed it.
type RoutingMetadata struct {
	Provider    string
	Deployment  string
	Node        string
	InjectUsage bool

	// Optional. Empty CostSource ⇒ entire cost block omitted; otherwise
	// CostUSD is emitted (including 0 for local providers).
	CostSource   CostSource
	CostUSD      float64
	LatencyMs    int64
	TTFTMs       int64
	TokensPerSec float64

	// Route-level observability (routes agent-control API). These fields
	// are populated by the dispatch chain after the candidate has been
	// selected and the fallback walk is complete. Empty values are
	// omitted from the wire so the body stays compact when no fallback
	// occurred and the dispatch path was trivial.
	GroupName         string
	StrategyUsed      string
	FallbackChain     []FallbackAttempt
	Skipped           []SkippedReplica
	DecisionLatencyMs int64

	// SteeringApplied reserves the wire slot for the per-call steering
	// header echo. Populated by the steering-header arc when those
	// headers land; an empty slice is omitted from the wire.
	SteeringApplied []string

	// SuppressUsageFrame, when true, drops the terminal usage-only frame
	// from a stream on its way to the client.
	//
	// Usage is requested from the engine unconditionally so the request can
	// be metered — a caller must not be able to opt out of being counted by
	// sending include_usage:false. This flag is how the caller's wire
	// contract is honoured anyway: they asked not to see usage, so they do
	// not, while the recorder still counts it. CopyWithMetrics observes each
	// chunk BEFORE the transforms run, so measurement happens upstream of
	// suppression and is unaffected.
	//
	// Set only by the hop facing the original client. A cluster-routed
	// request arrives at the worker already carrying include_usage:true, so
	// the worker leaves the frame alone and the coordinator — the node that
	// enforces spend — is the one that both counts and strips.
	SuppressUsageFrame bool

	// SuppressBodyMetadata, when true, skips the in-body "zzrouter"
	// block injection on non-streaming responses. Set from the
	// ?verbose=0 query param so high-RPS agents can save parse cost.
	// Response headers (X-zzrouter-*) still emit so observability is
	// preserved on the cheaper-to-read side of the wire.
	SuppressBodyMetadata bool

	// EstimatedCostMicro / Source reserve the wire slots for predictive
	// cost. Populated by the cross-route preview arc; zero source means
	// the cost block is omitted.
	EstimatedCostMicro  int64
	EstimatedCostSource string

	// ClientModel is the model id the caller used, when it differs from
	// the token the engine keys on. Every engine answers in its own
	// vocabulary -- Ollama echoes its registry name, which drops the
	// @node suffix the catalog advertises -- so the id in the response
	// stops matching any catalog entry. Set it and the response is
	// answered in the caller's own spelling. Empty leaves the engine's
	// answer alone.
	ClientModel string
}

// RewriteModelInBody replaces the "model" field in a JSON request body.
// Returns the original body unchanged if rewrite fails (best-effort).
func RewriteModelInBody(body []byte, newModel string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	modelBytes, err := json.Marshal(newModel)
	if err != nil {
		return body
	}
	raw["model"] = modelBytes

	rewritten, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return rewritten
}

// SetDefaultFields sets each top-level field of defaults the body does
// not already carry, and leaves every field the client sent as sent:
// a model's request defaults are defaults, not overrides. A field the
// client sent in another case counts as sent, since an engine decoding
// JSON case-insensitively would read both as one. A body that is not a
// JSON object, or that already has every field, is returned as is.
func SetDefaultFields(body []byte, defaults map[string]any) []byte {
	if len(defaults) == 0 {
		return body
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return body
	}
	sent := make(map[string]bool, len(raw))
	for k := range raw {
		sent[strings.ToLower(k)] = true
	}
	changed := false
	for k, v := range defaults {
		if sent[strings.ToLower(k)] {
			continue
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			continue
		}
		raw[k] = encoded
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// ForceUsageReporting sets stream_options.include_usage=true on a streaming
// request body, and reports whether the caller had already asked for it.
//
// It overwrites an explicit include_usage:false on purpose. Token counts are
// what the inference log meters and what budget settlement spends against, so
// a caller able to turn them off is a caller able to turn off being billed.
// The previous behaviour — leave any caller-supplied stream_options alone —
// meant `{"include_usage": false}` logged 0 in / 0 out on every path.
//
// The second return says whether the caller wanted to SEE usage, which is a
// separate question from whether we measure it. When false, the terminal
// usage frame is stripped on the way out (RoutingMetadata.SuppressUsageFrame)
// so the caller's wire contract is unchanged.
//
// Non-streaming bodies are returned untouched: usage is already in the
// response body there, and there is no frame to strip.
func ForceUsageReporting(body []byte) (out []byte, callerAsked bool) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}

	streaming, _ := obj["stream"].(bool)
	if !streaming {
		// Nothing to force and nothing to strip. Reporting "asked" keeps a
		// caller of this function from stripping frames on a path that has
		// none.
		return body, true
	}

	if opts, ok := obj["stream_options"].(map[string]any); ok {
		if asked, isBool := opts["include_usage"].(bool); isBool && asked {
			return body, true
		}
	}

	obj["stream_options"] = map[string]any{"include_usage": true}

	result, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return result, false
}

// applyExtras stamps the optional cost/timing fields onto target. The
// rule is "omit when CostSource is empty" for the cost block; timing
// fields are independently zero-gated.
func applyExtras(target map[string]any, m RoutingMetadata, prefix string) {
	if m.CostSource != "" {
		target[prefix+"cost_usd"] = m.CostUSD
		target[prefix+"cost_source"] = string(m.CostSource)
	}
	if m.LatencyMs > 0 {
		target[prefix+"latency_ms"] = m.LatencyMs
	}
	if m.TTFTMs > 0 {
		target[prefix+"ttft_ms"] = m.TTFTMs
	}
	if m.TokensPerSec > 0 {
		target[prefix+"tokens_per_second"] = m.TokensPerSec
	}
}

// applyRouteExtras stamps the route-level observability fields onto
// target. Empty / zero values are omitted so trivial dispatches don't
// pay the wire-noise cost.
func applyRouteExtras(target map[string]any, m RoutingMetadata) {
	if m.GroupName != "" {
		target["group_name"] = m.GroupName
	}
	if m.StrategyUsed != "" {
		target["strategy_used"] = m.StrategyUsed
	}
	if len(m.FallbackChain) > 0 {
		target["fallback_chain"] = m.FallbackChain
	}
	if len(m.Skipped) > 0 {
		target["skipped"] = m.Skipped
	}
	if m.DecisionLatencyMs > 0 {
		target["decision_latency_ms"] = m.DecisionLatencyMs
	}
	if len(m.SteeringApplied) > 0 {
		target["steering_applied"] = m.SteeringApplied
	}
	if m.EstimatedCostSource != "" {
		target["estimated_cost_micro"] = m.EstimatedCostMicro
		target["estimated_cost_source"] = m.EstimatedCostSource
	}
}

// InjectRoutingMetadata adds the root-level "zzrouter" block to a
// non-streaming response body. Streaming uses InjectUsageIntoSSEChunk
// + the usage.zz_* namespace; non-streaming uses the root block only.
// The two are deliberately disjoint — duplicating zz_* into usage on
// non-streaming responses is redundant noise (the root block already
// carries the same data) and confuses agents that read both shapes.
func InjectRoutingMetadata(body []byte, m RoutingMetadata) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}

	meta := map[string]any{"provider": m.Provider}
	if m.Deployment != "" {
		meta["deployment"] = m.Deployment
	}
	if m.Node != "" {
		meta["node"] = m.Node
	}
	applyExtras(meta, m, "")
	applyRouteExtras(meta, m)
	obj["zzrouter"] = meta

	result, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return result
}

// InjectUsageIntoSSEChunk scans SSE lines in a raw byte chunk and injects
// zz_* fields into any line that contains a usage object. Cost fields
// are stamped only on the terminal usage chunk (gated on
// usage.completion_tokens > 0 — partial usage chunks get routing fields
// only). Returns the modified chunk.
func InjectUsageIntoSSEChunk(chunk []byte, m RoutingMetadata) []byte {
	// Cheap scan before the split, which allocates. The copier offers
	// every chunk now, not just usage-bearing ones, so most calls have
	// nothing to do and should cost one pass rather than a slice header
	// per line.
	if !bytes.Contains(chunk, []byte(`"usage"`)) {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	modified := false
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data: ")) || bytes.Equal(trimmed, []byte("data: [DONE]")) {
			continue
		}
		if !bytes.Contains(trimmed, []byte(`"usage"`)) {
			continue
		}
		payload := trimmed[6:]
		var obj map[string]any
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		usage, ok := obj["usage"].(map[string]any)
		if !ok || usage == nil {
			continue
		}
		usage["zz_provider"] = m.Provider
		if m.Deployment != "" {
			usage["zz_model"] = m.Deployment
		}
		if m.Node != "" {
			usage["zz_node"] = m.Node
		}
		// Cost is non-idempotent across chunks. Stamp it only on the
		// terminal usage frame, identified by completion_tokens > 0.
		if isTerminalUsage(usage) {
			applyExtras(usage, m, "zz_")
		}
		if result, err := json.Marshal(obj); err == nil {
			lines[i] = append([]byte("data: "), result...)
			modified = true
		}
	}
	if !modified {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

// ChunkHasUsageObject reports whether an SSE chunk carries a non-null
// usage object on at least one data frame.
//
// A substring test for `"usage"` is not enough: OpenAI and vLLM emit
// `"usage":null` on every delta frame, so a caller counting on the
// substring concludes usage arrived on frame one and never notices that
// none ever did.
func ChunkHasUsageObject(chunk []byte) bool {
	if !bytes.Contains(chunk, []byte(`"usage"`)) {
		return false
	}
	for line := range bytes.SplitSeq(chunk, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data: ")) || bytes.Equal(trimmed, []byte("data: [DONE]")) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(trimmed[6:], &obj) != nil {
			continue
		}
		if usage, ok := obj["usage"].(map[string]any); ok && usage != nil {
			return true
		}
	}
	return false
}

// isTerminalUsage reports whether a parsed usage map looks like the
// final usage frame in a stream — i.e. carries non-zero
// completion_tokens. OpenAI's contract emits usage exactly once, in
// the final chunk; other gateways occasionally emit partial usage
// frames mid-stream that should not carry cost.
func isTerminalUsage(usage map[string]any) bool {
	switch v := usage["completion_tokens"].(type) {
	case float64:
		return v > 0
	case int64:
		return v > 0
	case int:
		return v > 0
	case json.Number:
		f, err := v.Float64()
		return err == nil && f > 0
	}
	return false
}

// SetModelIfPresent replaces the "model" field when the payload already
// carries one. A payload without a model is returned untouched: this
// restores a name, it never invents one.
func SetModelIfPresent(body []byte, newModel string) []byte {
	// Returning early when the name already matches is what keeps this
	// cheap on the streaming path, where it now runs per frame: the
	// rewrite costs an unmarshal, a re-marshal and a fresh map even when
	// it would change nothing.
	if cur := ModelFromBody(body); cur == "" || cur == newModel {
		return body
	}
	return RewriteModelInBody(body, newModel)
}

// ModelFromBody returns the "model" field of a JSON payload, or "" when the
// payload isn't JSON or carries no model.
func ModelFromBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
}

// SSE framing tokens. Declared once so the parser and any future frame
// handling agree on the exact bytes.
var (
	sseDataPrefix = []byte("data: ")
	sseDoneFrame  = []byte("data: [DONE]")
)

// RewriteModelInStreamChunk replaces the "model" field of every JSON frame
// in a streamed chunk, in either framing this proxy carries: SSE
// `data: {...}` lines on /v1/*, and bare NDJSON objects on /api/*.
//
// Both are handled here rather than in two functions because the callers
// cannot tell them apart — one transform is built per response and the
// framing is the upstream's choice, so an SSE-only rewriter silently did
// nothing to every Ollama stream.
//
// Frames without a model are left untouched, so this never invents the
// field on payloads that don't carry it.
func RewriteModelInStreamChunk(chunk []byte, newModel string) []byte {
	// Same cheap scan as InjectUsageIntoSSEChunk: skip the allocating
	// split for frames that name no model, which is every keep-alive
	// comment and the terminal [DONE].
	if !bytes.Contains(chunk, []byte(`"model"`)) {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	modified := false
	for i, line := range lines {
		payload, at := streamFrameJSON(line)
		if payload == nil || !bytes.Contains(payload, []byte(`"model"`)) {
			continue
		}
		rewritten := SetModelIfPresent(payload, newModel)
		// SetModelIfPresent returns its input unchanged when the payload
		// does not parse or already names the caller's model, so equality
		// -- not nil -- is what says "nothing to do". Leaving the line
		// untouched in that case is the whole safety property here: a
		// frame split across a read boundary arrives as unparseable JSON,
		// and rebuilding it would strip the whitespace that its
		// continuation needs in order to reassemble.
		if bytes.Equal(rewritten, payload) {
			continue
		}
		// Splice into the original line rather than emitting prefix+JSON,
		// so indentation and a trailing CR survive a rewrite.
		out := make([]byte, 0, len(line)-len(payload)+len(rewritten))
		out = append(out, line[:at]...)
		out = append(out, rewritten...)
		lines[i] = append(out, line[at+len(payload):]...)
		modified = true
	}
	if !modified {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

// StripUsageOnlyFrames removes the terminal usage-only frame from a stream
// chunk — the `{"choices":[],"usage":{…}}` frame that include_usage adds.
//
// Used when usage was requested from the engine for metering but the caller
// did not ask to see it. CopyWithMetrics observes each chunk before running
// the transforms, so the tokens are already recorded by the time this drops
// the frame.
//
// Only frames with an EMPTY choices array and a usage object are dropped.
// Some providers attach usage to the final content frame instead; dropping
// that would take the caller's last token with it.
//
// A line that does not parse is left alone, the same rule
// RewriteModelInStreamChunk follows. CopyWithMetrics hands the transforms
// whole lines, so that is a floor rather than an expected case: dropping a
// fragment would delete half a frame its continuation still expects.
func StripUsageOnlyFrames(chunk []byte) []byte {
	// Same cheap scan as the other stream transforms: the frames that carry
	// usage are a vanishing fraction of a stream.
	if !bytes.Contains(chunk, []byte(`"usage"`)) {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	kept := make([][]byte, 0, len(lines))
	dropped := false
	for _, line := range lines {
		payload, _ := streamFrameJSON(line)
		if payload != nil && isUsageOnlyFrame(payload) {
			dropped = true
			continue
		}
		kept = append(kept, line)
	}
	if !dropped {
		return chunk
	}
	return bytes.Join(kept, []byte("\n"))
}

// isUsageOnlyFrame reports whether payload is a frame whose entire purpose is
// carrying usage — no choices, some usage.
func isUsageOnlyFrame(payload []byte) bool {
	var probe struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return len(probe.Choices) == 0 && len(probe.Usage) > 0
}

// frameSpace is the whitespace that surrounds a frame's payload on the
// wire. Trimmed for parsing, preserved on output.
const frameSpace = " \t\r"

// streamFrameJSON locates the JSON payload inside one line of a streamed
// body and returns it with its offset in that line, so a rewrite can be
// spliced back without disturbing the framing bytes around it. Returns a
// nil payload for anything that is not a JSON frame: blank separators,
// SSE comments, and the [DONE] sentinel.
func streamFrameJSON(line []byte) (payload []byte, at int) {
	lead := len(line) - len(bytes.TrimLeft(line, frameSpace))
	body := bytes.TrimRight(line[lead:], frameSpace)

	if bytes.Equal(body, sseDoneFrame) {
		return nil, 0
	}
	if rest, ok := bytes.CutPrefix(body, sseDataPrefix); ok {
		return rest, lead + len(sseDataPrefix)
	}
	// NDJSON: the line is the object.
	if len(body) > 0 && body[0] == '{' {
		return body, lead
	}
	return nil, 0
}
