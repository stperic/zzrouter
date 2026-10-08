package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stperic/zzrouter/pkg/observability/llm"
)

// UsageData holds extracted token counts and extended usage from a
// response.
type UsageData struct {
	In              int64
	Out             int64
	CachedTokens    int64
	ReasoningTokens int64
	Cost            float64
	// CostSource is non-empty when the upstream returned an
	// authoritative cost number (CostSourceProvider). Empty means the
	// caller (or quota.CalculateCostMicro) decides — pricing-store
	// compute or omit.
	CostSource CostSource
	// Model is the response.model field — what the upstream backend
	// reports having actually used. Distinct from request.model for
	// model-group routing (alias resolves to a concrete deployment) and
	// for backends that auto-version (e.g. "gpt-4-turbo" -> "gpt-4-turbo-2024-04-09").
	// Surfaced as `gen_ai.response.model` on metrics + spans.
	Model string
}

// merge folds a later usage frame into the totals seen so far. Usage in
// a stream is cumulative in every vocabulary we read (OpenAI continuous
// usage, Anthropic's message_start then message_delta), so a count never
// legitimately falls: each field keeps its maximum. A frame that reports
// only part of the picture -- output tokens alone, or input without the
// cache counts -- therefore cannot erase what an earlier frame said.
func (u UsageData) merge(later UsageData) UsageData {
	u.In = max(u.In, later.In)
	u.Out = max(u.Out, later.Out)
	u.CachedTokens = max(u.CachedTokens, later.CachedTokens)
	u.ReasoningTokens = max(u.ReasoningTokens, later.ReasoningTokens)
	if later.Cost > u.Cost {
		u.Cost, u.CostSource = later.Cost, later.CostSource
	}
	if later.Model != "" {
		u.Model = later.Model
	}
	return u
}

// jsonNumber handles both int and float JSON numbers for token counts.
type jsonNumber float64

// Int64 reads a token count; an absent (nil) field counts as zero.
func (n *jsonNumber) Int64() int64 {
	if n == nil {
		return 0
	}
	return int64(*n)
}

// ParseUsageExtended extracts full usage including cached/reasoning
// tokens and cost. Uses a single JSON parse to extract both standard
// usage fields and provider-specific cost extensions (x_openrouter,
// x_cloudflare, etc.).
//
// Three token vocabularies share the "usage" key. OpenAI Chat uses
// prompt_tokens/completion_tokens. The Responses API and Anthropic
// Messages both use input_tokens/output_tokens, but Anthropic counts
// cache reads and writes outside input_tokens, so they are added back:
// In always means every prompt token, as RecordCompletion expects.
// Anthropic's streaming message_start nests the block under "message".
func ParseUsageExtended(body []byte) UsageData {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return UsageData{}
	}
	if msgRaw, ok := raw["message"]; ok {
		if _, hasUsage := raw["usage"]; !hasUsage {
			var msg map[string]json.RawMessage
			if json.Unmarshal(msgRaw, &msg) == nil && msg["usage"] != nil {
				raw = msg
			}
		}
	}

	// Extract the response model regardless of whether usage is present —
	// streaming-chunk error frames may carry model + no usage and the
	// recorder still benefits from the response.model attribute.
	// Non-string model values (number, object, null) leave responseModel
	// at "", and the downstream `if u.Model != ""` gate filters those
	// out so malformed upstreams can't pollute the label dimension.
	var responseModel string
	if mRaw, ok := raw["model"]; ok {
		_ = json.Unmarshal(mRaw, &responseModel)
	}

	usageRaw, ok := raw["usage"]
	if !ok {
		// Return Model-only if it's the only field worth surfacing.
		if responseModel != "" {
			return UsageData{Model: responseModel}
		}
		return UsageData{}
	}

	type tokenDetails struct {
		CachedTokens    jsonNumber `json:"cached_tokens"`
		ReasoningTokens jsonNumber `json:"reasoning_tokens"`
	}
	var usage struct {
		PromptTokens            *jsonNumber   `json:"prompt_tokens"`
		CompletionTokens        *jsonNumber   `json:"completion_tokens"`
		PromptTokensDetails     *tokenDetails `json:"prompt_tokens_details"`
		CompletionTokensDetails *tokenDetails `json:"completion_tokens_details"`

		InputTokens              jsonNumber    `json:"input_tokens"`
		OutputTokens             jsonNumber    `json:"output_tokens"`
		CacheReadInputTokens     jsonNumber    `json:"cache_read_input_tokens"`
		CacheCreationInputTokens jsonNumber    `json:"cache_creation_input_tokens"`
		InputTokensDetails       *tokenDetails `json:"input_tokens_details"`
		OutputTokensDetails      *tokenDetails `json:"output_tokens_details"`

		Cost jsonNumber `json:"cost"`
	}
	if err := json.Unmarshal(usageRaw, &usage); err != nil {
		return UsageData{}
	}

	u := UsageData{Model: responseModel}
	switch {
	// A gateway that echoes several vocabularies is read as Chat: the
	// presence of a Chat field decides, not which count is larger.
	case usage.PromptTokens != nil || usage.CompletionTokens != nil:
		u.In = usage.PromptTokens.Int64()
		u.Out = usage.CompletionTokens.Int64()
		if usage.PromptTokensDetails != nil {
			u.CachedTokens = usage.PromptTokensDetails.CachedTokens.Int64()
		}
		if usage.CompletionTokensDetails != nil {
			u.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens.Int64()
		}
	// Responses: input_tokens already includes the cached share.
	case usage.InputTokensDetails != nil:
		u.In = usage.InputTokens.Int64()
		u.Out = usage.OutputTokens.Int64()
		u.CachedTokens = usage.InputTokensDetails.CachedTokens.Int64()
		if usage.OutputTokensDetails != nil {
			u.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens.Int64()
		}
	// Anthropic: cache reads and writes sit outside input_tokens.
	default:
		cacheRead := usage.CacheReadInputTokens.Int64()
		u.In = usage.InputTokens.Int64() + cacheRead + usage.CacheCreationInputTokens.Int64()
		u.Out = usage.OutputTokens.Int64()
		u.CachedTokens = cacheRead
	}

	// OpenRouter and (per current spec) every gateway that surfaces an
	// authoritative cost puts it at usage.cost. The legacy x_*.cost
	// extension shape is checked as a fallback for older gateway DTOs.
	// In both arms a non-zero value sets CostSource = provider so
	// downstream consumers know the number is upstream-authoritative.
	if usage.Cost > 0 {
		u.Cost = float64(usage.Cost)
		u.CostSource = CostSourceProvider
	} else if c := extractProviderCost(raw); c > 0 {
		u.Cost = c
		u.CostSource = CostSourceProvider
	}
	return u
}

// extractProviderCost looks for a cost value in any provider-specific
// extension object. Cloud providers embed cost in fields like
// "x_openrouter": {"cost": 0.001}. This handles any "x_*" field with a
// "cost" sub-field, so new providers work automatically.
func extractProviderCost(raw map[string]json.RawMessage) float64 {
	for key, val := range raw {
		if len(key) < 3 || key[:2] != "x_" {
			continue
		}
		var ext struct {
			Cost jsonNumber `json:"cost"`
		}
		if err := json.Unmarshal(val, &ext); err == nil && ext.Cost != 0 {
			return float64(ext.Cost)
		}
	}
	return 0
}

// ParseOllamaUsage extracts token counts from an Ollama JSON response.
// Only returns data when done=true (final response).
func ParseOllamaUsage(body []byte) (int64, int64, bool) {
	in, out, _, ok := parseOllamaResponse(body)
	return in, out, ok
}

// ParseOllamaUsageExtended is the model-aware companion to
// ParseOllamaUsage. Returns (in, out, model, ok) so callers that need
// to populate gen_ai.response.model don't have to re-parse the body.
func ParseOllamaUsageExtended(body []byte) (int64, int64, string, bool) {
	return parseOllamaResponse(body)
}

func parseOllamaResponse(body []byte) (int64, int64, string, bool) {
	var resp struct {
		Model           string     `json:"model"`
		Done            bool       `json:"done"`
		PromptEvalCount jsonNumber `json:"prompt_eval_count"`
		EvalCount       jsonNumber `json:"eval_count"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || !resp.Done {
		return 0, 0, "", false
	}
	in := resp.PromptEvalCount.Int64()
	out := resp.EvalCount.Int64()
	if in == 0 && out == 0 {
		return 0, 0, "", false
	}
	return in, out, resp.Model, true
}

// ParseSSEFinalUsageExtended extracts full usage data from an SSE chunk.
func ParseSSEFinalUsageExtended(chunk []byte) UsageData {
	data := bytes.TrimSpace(chunk)

	// OpenAI SSE format: "data: {...}"
	if after, ok := bytes.CutPrefix(data, []byte("data: ")); ok {
		data = after
		if bytes.Equal(data, []byte("[DONE]")) {
			return UsageData{}
		}
		return ParseUsageExtended(data)
	}

	// Ollama NDJSON format: bare JSON objects
	if in, out, model, ok := ParseOllamaUsageExtended(data); ok {
		return UsageData{In: in, Out: out, Model: model}
	}
	return UsageData{}
}

// CaptureNonStreamingMetrics reads the response body for metrics
// extraction, then replaces it so downstream consumers can still read
// it. Handles both Content-Length and chunked transfer encoding
// responses. When resp.StatusCode indicates failure (>= 400) the
// recorder is tagged with a closed-enum error_type before
// RecordCompletion fires, so OTel emission carries the failure label.
func CaptureNonStreamingMetrics(resp *http.Response, recorder *llm.InferenceRecorder) {
	// Tag error_type from status BEFORE the body read — applies even
	// when the body is too large or empty for usage extraction.
	tagRecorderFromStatus(recorder, resp.StatusCode)

	// Accept responses with known size < 1 MiB, or chunked (ContentLength == -1)
	if resp.ContentLength > 1<<20 {
		// Too large to extract usage — but for error responses we still
		// need RecordCompletion to fire so the metric emits with the
		// error_type tag. The extracted-usage path can stay the
		// success contract; here we cover the pure-error case.
		if recorder != nil && resp.StatusCode >= 400 {
			recorder.RecordCompletion(0, 0)
		}
		return
	}
	if resp.ContentLength == 0 {
		if recorder != nil && resp.StatusCode >= 400 {
			recorder.RecordCompletion(0, 0)
		}
		return
	}

	// Read body (with a 1 MiB safety limit for chunked responses)
	limitReader := io.LimitReader(resp.Body, 1<<20)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil || len(bodyBytes) == 0 {
		if recorder != nil && resp.StatusCode >= 400 {
			recorder.RecordCompletion(0, 0)
		}
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	// The body is already in hand for usage parsing, so capturing the
	// reply here costs no extra read.
	recorder.SetResponseBody(bodyBytes)
	recorder.AppendResponseText(ResponseText(bodyBytes))
	recordProxyMetrics(bodyBytes, recorder)
}

// tagRecorderFromStatus sets the recorder's error_type from an upstream
// HTTP status code. Closed-enum mapping keeps Prometheus cardinality
// predictable (any 5xx → upstream_server_error, etc.) and lets
// dashboards split error rates without learning every backend's status
// vocabulary. No-op for 2xx and 3xx.
func tagRecorderFromStatus(recorder *llm.InferenceRecorder, statusCode int) {
	if recorder == nil || statusCode < 400 {
		return
	}
	errType := errorTypeFromStatus(statusCode)
	recorder.SetError(errType, fmt.Sprintf("upstream returned HTTP %d", statusCode))
}

// errorTypeFromStatus maps an upstream HTTP status code to a closed-enum
// error_type label. Exported via SetErrorFromStatus for callers outside
// the wire package that need the same mapping (proxy clients tagging
// before they hand off to wire). 429 is split out because rate-limit
// dashboards routinely watch it as a distinct signal from generic 4xx.
func errorTypeFromStatus(statusCode int) string {
	switch {
	case statusCode == 429:
		return "upstream_rate_limited"
	case statusCode >= 500:
		return "upstream_server_error"
	case statusCode >= 400:
		return "upstream_client_error"
	}
	return ""
}

// SetErrorFromStatus is the exported form of the status → error_type
// tagger for callers outside the wire package (proxy clients that
// short-circuit before entering the wire layer). Applies the same
// closed-enum mapping as CaptureNonStreamingMetrics. No-op when
// recorder is nil or the status doesn't indicate failure.
func SetErrorFromStatus(recorder *llm.InferenceRecorder, statusCode int) {
	tagRecorderFromStatus(recorder, statusCode)
}

// RecordPreByteCopyError is the canonical helper for tagging a
// recorder + emitting the OTel GenAI series on abort paths that never
// reach the wire layer's own RecordCompletion (transport failure,
// proxy build error, HTML mis-route, fallback exhaustion). One call
// site, one shape — replaces the ad-hoc SetError + RecordCompletion(0,0)
// pairs that used to live in proxy_client.go and chain/fallback.go.
//
// Calling this on a path that ALSO reaches the wire layer's
// CaptureNonStreamingMetrics or CopyWithMetrics would double-emit the
// operation_duration histogram (and double-fire the inference log
// bridge — settling spend twice). Use only on pre-byte-copy aborts
// where no further metric path runs. Safe no-op when recorder is nil.
func RecordPreByteCopyError(recorder *llm.InferenceRecorder, errType, message string) {
	if recorder == nil {
		return
	}
	recorder.SetError(errType, message)
	recorder.RecordCompletion(0, 0)
}

// RecordPreByteCopyErrorFromStatus is the status-driven companion to
// RecordPreByteCopyError. Picks the closed-enum error_type from the
// upstream HTTP status code and fires SetError + RecordCompletion(0,0)
// in one call. Used by the proxy 4xx/5xx pre-rewrite path where the
// caller knows the status but not a specific error message. No-op
// when recorder is nil or status doesn't indicate failure.
func RecordPreByteCopyErrorFromStatus(recorder *llm.InferenceRecorder, statusCode int) {
	if recorder == nil || statusCode < 400 {
		return
	}
	errType := errorTypeFromStatus(statusCode)
	recorder.SetError(errType, fmt.Sprintf("upstream returned HTTP %d", statusCode))
	recorder.RecordCompletion(0, 0)
}

// recordProxyMetrics records metrics from a completed proxy response.
// Caller is expected to have already tagged the recorder via
// tagRecorderFromStatus when the response indicates failure — this
// function is the success-shape extractor.
func recordProxyMetrics(respBody []byte, recorder *llm.InferenceRecorder) {
	if recorder == nil {
		return
	}

	// Try OpenAI format first (with extended usage extraction).
	// SetResponseModel runs whenever the body carries a model field —
	// even when usage is absent (e.g. a 4xx envelope that still echoes
	// the model). gen_ai.response.model lands on the span/metric
	// independently of token counts.
	u := ParseUsageExtended(respBody)
	if u.Model != "" {
		recorder.SetResponseModel(u.Model)
	}
	if u.In > 0 || u.Out > 0 {
		recorder.SetExtendedUsage(u.CachedTokens, u.ReasoningTokens, u.Cost, string(u.CostSource))
		recorder.RecordCompletion(u.In, u.Out)
		return
	}

	// Try Ollama format
	if in, out, model, ok := ParseOllamaUsageExtended(respBody); ok {
		if model != "" {
			recorder.SetResponseModel(model)
		}
		recorder.RecordCompletion(in, out)
		return
	}

	recorder.RecordCompletion(0, 0)
}
