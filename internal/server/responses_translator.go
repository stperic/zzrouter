// Translation shim for /v1/responses against providers whose
// wire_endpoints does not declare "responses". Mirrors the bridge
// LiteLLM ships in Python (LiteLLMResponsesTransformationHandler):
// rewrite the incoming Responses body into a Chat Completions body,
// dispatch through the existing chat path, and translate the upstream
// chat envelope back into the Responses shape on the way out.
//
// The translator is split across three concerns:
//
//   - translateResponsesRequest: parse a Responses create body, reject
//     parameters that have no Chat-Completions equivalent (store=true,
//     previous_response_id, background, built-in tools), and return a
//     Chat-shaped body.
//
//   - translateChatToResponses: serialize a non-streaming Chat
//     completion JSON into a Responses envelope (output_text +
//     function_call output items, usage block).
//
//   - responsesStreamWriter: line-buffered SSE state machine that
//     emits the response.* event sequence
//     (response.created → response.in_progress → output_item.added
//     → content_part.added → output_text.delta* → content_part.done
//     → output_item.done → response.completed) from a stream of
//     chat.completion.chunk frames.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/utils"
)

// invalidRequestErrType is the OpenAI error type for client-side
// validation failures hit by the translation shim.
const invalidRequestErrType = "invalid_request_error"

// maxStreamBufBytes caps the line-buffered SSE accumulator so a
// malicious or buggy upstream that streams without `\n\n` cannot
// exhaust memory. On overflow, the stream writer emits response.failed
// and stops translating further frames.
const maxStreamBufBytes = 1 << 20 // 1 MiB

// ---------- Request types (Responses subset) ----------

// responsesRequest captures the fields zzRouter understands on the
// incoming /v1/responses body. Unknown fields are tolerated for
// forward compatibility but not forwarded — the upstream is Chat
// Completions, which has its own surface.
type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
	Text              json.RawMessage `json:"text,omitempty"`
	User              string          `json:"user,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`

	// Rejected-with-400 in the shim path:
	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Background         *bool           `json:"background,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
}

// translateError carries a structured 400 envelope back to handleResponses
// so the caller can write it without rebuilding the OpenAI shape.
type translateError struct {
	Code    string
	Param   string
	Message string
}

func (e *translateError) write(c *gin.Context) {
	var paramPtr *string
	if e.Param != "" {
		p := e.Param
		paramPtr = &p
	}
	var codePtr *string
	if e.Code != "" {
		code := e.Code
		codePtr = &code
	}
	c.JSON(http.StatusBadRequest, utils.NewOpenAIError(invalidRequestErrType, e.Message, paramPtr, codePtr))
}

// translateResponsesRequest converts a Responses create body into a
// Chat Completions create body. The returned body is ready to drop
// into the chat dispatcher. Parameters that have no chat-completions
// equivalent yield a *translateError; nil error otherwise.
func translateResponsesRequest(body []byte) ([]byte, *responsesRequest, *translateError) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, &translateError{
			Code:    "invalid_json",
			Param:   "",
			Message: fmt.Sprintf("Invalid JSON: %v", err),
		}
	}

	if req.Store != nil && *req.Store {
		return nil, nil, unsupported("store",
			"store=true requires backend-side response persistence which the translation shim does not provide. Use a provider that natively serves /v1/responses, or omit the field.")
	}
	if req.PreviousResponseID != "" {
		return nil, nil, unsupported("previous_response_id",
			"previous_response_id requires backend-side state which the translation shim does not provide. Carry conversation turns explicitly via input items.")
	}
	if req.Background != nil && *req.Background {
		return nil, nil, unsupported("background",
			"background mode requires backend-side response persistence which the translation shim does not provide.")
	}

	chat := map[string]any{}
	if req.Model != "" {
		chat["model"] = req.Model
	}

	messages, terr := translateInputToMessages(req.Input, req.Instructions)
	if terr != nil {
		return nil, nil, terr
	}
	chat["messages"] = messages

	if len(req.Tools) > 0 {
		tools, terr := translateTools(req.Tools)
		if terr != nil {
			return nil, nil, terr
		}
		chat["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		chat["tool_choice"] = req.ToolChoice
	}
	if req.ParallelToolCalls != nil {
		chat["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		chat["max_completion_tokens"] = *req.MaxOutputTokens
	}
	if req.Stream {
		chat["stream"] = true
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.ResponseFormat) > 0 {
		chat["response_format"] = req.ResponseFormat
	} else if rf := translateTextFormat(req.Text); rf != nil {
		chat["response_format"] = rf
	}
	if req.User != "" {
		chat["user"] = req.User
	}

	out, err := json.Marshal(chat)
	if err != nil {
		return nil, nil, &translateError{
			Code:    "translation_failed",
			Message: fmt.Sprintf("Failed to marshal translated body: %v", err),
		}
	}
	return out, &req, nil
}

func unsupported(param, msg string) *translateError {
	return &translateError{
		Code:    "unsupported_parameter",
		Param:   param,
		Message: msg,
	}
}

// translateInputToMessages converts the Responses `input` field into a
// chat messages array. `input` is polymorphic per spec: a bare string,
// or an array of Items. Items handled:
//
//   - {type:"message", role, content:[{type:"input_text"|"output_text"|"input_image", ...}]}
//   - bare {role, content} (LiteLLM-style flat shorthand)
//   - {type:"function_call", call_id, name, arguments}
//   - {type:"function_call_output", call_id, output}
//   - {type:"reasoning", ...} → dropped with a sentinel system note
//
// `instructions` becomes a leading system message when non-empty.
func translateInputToMessages(input json.RawMessage, instructions string) ([]map[string]any, *translateError) {
	var messages []map[string]any
	if instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	if len(input) == 0 {
		return messages, nil
	}

	// Bare string shorthand: input: "hello".
	if len(input) > 0 && input[0] == '"' {
		var s string
		if err := json.Unmarshal(input, &s); err == nil {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": s,
			})
			return messages, nil
		}
	}

	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		// Single object form: input: {role: ..., content: ...}.
		var single map[string]any
		if err2 := json.Unmarshal(input, &single); err2 == nil {
			items = []json.RawMessage{input}
		} else {
			return nil, &translateError{
				Code:    "invalid_input",
				Param:   "input",
				Message: fmt.Sprintf("input must be a string or array of items: %v", err),
			}
		}
	}

	for i, raw := range items {
		var probe struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			CallID  string          `json:"call_id"`
			Name    string          `json:"name"`
			// function_call.arguments is a string per Responses spec.
			Arguments string `json:"arguments"`
			// function_call_output.output is a string per Responses spec.
			Output string `json:"output"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, &translateError{
				Code:    "invalid_input",
				Param:   fmt.Sprintf("input[%d]", i),
				Message: fmt.Sprintf("item is not a valid object: %v", err),
			}
		}

		switch probe.Type {
		case "function_call":
			// Assistant tool-call turn. Chat format wants a single
			// assistant message carrying tool_calls[].
			toolCall := map[string]any{
				"id":   probe.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      probe.Name,
					"arguments": probe.Arguments,
				},
			}
			messages = append(messages, map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": []any{toolCall},
			})
		case "function_call_output":
			// LiteLLM bug #18226: tool message content must be a string,
			// never a list. Responses ships a string already, so direct
			// passthrough is correct.
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": probe.CallID,
				"content":      probe.Output,
			})
		case "reasoning":
			// No chat-completions analogue; silently drop.
			continue
		case "message", "":
			role := probe.Role
			if role == "" {
				role = "user"
			}
			content, terr := translateMessageContent(probe.Content, i)
			if terr != nil {
				return nil, terr
			}
			messages = append(messages, map[string]any{
				"role":    role,
				"content": content,
			})
		default:
			return nil, &translateError{
				Code:    "unsupported_input_item",
				Param:   fmt.Sprintf("input[%d].type", i),
				Message: fmt.Sprintf("input item type %q is not supported by the translation shim", probe.Type),
			}
		}
	}
	return messages, nil
}

// translateMessageContent maps the Responses content array
// ([{type:"input_text", text}], [{type:"input_image", image_url}], …)
// to the Chat Completions content shape. A single input_text Item
// collapses to a plain string; mixed/multiple parts emit the Chat
// vision-style array ([{type:"text", text}], [{type:"image_url",
// image_url:{url}}]).
func translateMessageContent(content json.RawMessage, idx int) (any, *translateError) {
	if len(content) == 0 {
		return "", nil
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			return s, nil
		}
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, &translateError{
			Code:    "invalid_input",
			Param:   fmt.Sprintf("input[%d].content", idx),
			Message: fmt.Sprintf("content must be a string or array of parts: %v", err),
		}
	}

	if len(parts) == 1 {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(parts[0], &p); err == nil {
			if (p.Type == "input_text" || p.Type == "output_text") && p.Text != "" {
				return p.Text, nil
			}
		}
	}

	out := make([]map[string]any, 0, len(parts))
	for j, raw := range parts {
		var p struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			ImageURL json.RawMessage `json:"image_url"`
			Detail   string          `json:"detail,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &translateError{
				Code:    "invalid_input",
				Param:   fmt.Sprintf("input[%d].content[%d]", idx, j),
				Message: fmt.Sprintf("content part is not a valid object: %v", err),
			}
		}
		switch p.Type {
		case "input_text", "output_text", "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			// Responses: {type:"input_image", image_url:"https://..."}
			// Chat: {type:"image_url", image_url:{url:"..."}}
			var url string
			if len(p.ImageURL) > 0 {
				if p.ImageURL[0] == '"' {
					_ = json.Unmarshal(p.ImageURL, &url)
				} else {
					var imgObj struct {
						URL string `json:"url"`
					}
					_ = json.Unmarshal(p.ImageURL, &imgObj)
					url = imgObj.URL
				}
			}
			img := map[string]any{"url": url}
			if p.Detail != "" {
				img["detail"] = p.Detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": img})
		default:
			return nil, &translateError{
				Code:    "unsupported_content_part",
				Param:   fmt.Sprintf("input[%d].content[%d].type", idx, j),
				Message: fmt.Sprintf("content part type %q is not supported by the translation shim", p.Type),
			}
		}
	}
	return out, nil
}

// translateTools converts Responses flat tool descriptors
// {type, name, description, parameters} into Chat's nested form
// {type:"function", function:{name, description, parameters}}. Built-in
// tools (web_search, file_search, code_interpreter, computer_use) are
// rejected — they require backend-side execution the shim cannot mediate.
func translateTools(raw json.RawMessage) ([]map[string]any, *translateError) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &translateError{
			Code:    "invalid_tools",
			Param:   "tools",
			Message: fmt.Sprintf("tools must be an array: %v", err),
		}
	}
	out := make([]map[string]any, 0, len(items))
	for i, t := range items {
		var probe struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			// Already-nested {type:"function", function:{...}} is also
			// accepted as a passthrough for clients that pre-translate.
			Function json.RawMessage `json:"function"`
		}
		if err := json.Unmarshal(t, &probe); err != nil {
			return nil, &translateError{
				Code:    "invalid_tools",
				Param:   fmt.Sprintf("tools[%d]", i),
				Message: fmt.Sprintf("tool entry is not a valid object: %v", err),
			}
		}
		switch probe.Type {
		case "function":
			if len(probe.Function) > 0 {
				out = append(out, map[string]any{
					"type":     "function",
					"function": probe.Function,
				})
				continue
			}
			fn := map[string]any{"name": probe.Name}
			if probe.Description != "" {
				fn["description"] = probe.Description
			}
			if len(probe.Parameters) > 0 {
				fn["parameters"] = probe.Parameters
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		case "web_search", "web_search_preview", "file_search", "code_interpreter", "computer_use_preview":
			return nil, &translateError{
				Code:    "unsupported_tool",
				Param:   fmt.Sprintf("tools[%d].type", i),
				Message: fmt.Sprintf("built-in tool %q requires backend-side execution which the translation shim does not provide", probe.Type),
			}
		default:
			return nil, &translateError{
				Code:    "unsupported_tool",
				Param:   fmt.Sprintf("tools[%d].type", i),
				Message: fmt.Sprintf("tool type %q is not supported", probe.Type),
			}
		}
	}
	return out, nil
}

// translateTextFormat folds the Responses `text.format` shape into Chat's
// `response_format`. Best-effort — passes the field through verbatim
// when the inner shape already matches.
func translateTextFormat(text json.RawMessage) any {
	if len(text) == 0 {
		return nil
	}
	var probe struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(text, &probe); err != nil || len(probe.Format) == 0 {
		return nil
	}
	return probe.Format
}

// ---------- Non-streaming response translation ----------

// chatCompletionEnvelope is the upstream shape we read; mirrors only
// the fields the translator extracts.
type chatCompletionEnvelope struct {
	ID       string          `json:"id"`
	Created  int64           `json:"created"`
	Model    string          `json:"model"`
	Choices  []chatChoice    `json:"choices"`
	Usage    json.RawMessage `json:"usage"`
	Zzrouter json.RawMessage `json:"zzrouter,omitempty"`
}

type chatChoice struct {
	Index        int             `json:"index"`
	Message      chatMessageOut  `json:"message"`
	FinishReason string          `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

type chatMessageOut struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// translateChatToResponses serializes a non-streaming Chat Completions
// envelope into a Responses envelope.
//
// LiteLLM bug #18226: multi-tool-call requests must collapse into a
// single Responses output[] containing N function_call Items rather
// than fanning out into separate choices/responses. We honor that by
// flattening choices[0].message.tool_calls[] into output items in
// order.
func translateChatToResponses(chatJSON []byte, requestModel string) ([]byte, error) {
	var env chatCompletionEnvelope
	if err := json.Unmarshal(chatJSON, &env); err != nil {
		return nil, fmt.Errorf("upstream chat envelope did not parse: %w", err)
	}
	if len(env.Choices) == 0 {
		return nil, fmt.Errorf("upstream chat envelope had no choices")
	}

	respID := env.ID
	if respID == "" || strings.HasPrefix(respID, "chatcmpl-") {
		respID = "resp_" + strings.TrimPrefix(respID, "chatcmpl-")
		if respID == "resp_" {
			respID = "resp_" + strconv.FormatInt(utils.Now().UnixNano(), 36)
		}
	}

	choice := env.Choices[0]
	output := make([]map[string]any, 0, 1+len(choice.Message.ToolCalls))

	if choice.Message.Content != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + respID,
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{
				{
					"type":        "output_text",
					"text":        choice.Message.Content,
					"annotations": []any{},
				},
			},
		})
	}
	for _, tc := range choice.Message.ToolCalls {
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + tc.ID,
			"call_id":   tc.ID,
			"name":      tc.Function.Name,
			"arguments": tc.Function.Arguments,
			"status":    "completed",
		})
	}

	status, incompleteReason := mapFinishToStatus(choice.FinishReason)
	resp := map[string]any{
		"id":          respID,
		"object":      "response",
		"created_at":  env.Created,
		"model":       firstNonEmpty(env.Model, requestModel),
		"status":      status,
		"output":      output,
		"output_text": choice.Message.Content,
	}
	if incompleteReason != "" {
		resp["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	if len(env.Usage) > 0 {
		resp["usage"] = translateUsage(env.Usage)
	}
	// Pass the zzrouter routing/cost block through unchanged. The chat
	// inject path stamped it onto the upstream-shape envelope; the
	// Responses-shape envelope inherits the same provider/node/cost
	// provenance since the request hit the same backend.
	if len(env.Zzrouter) > 0 {
		resp["zzrouter"] = env.Zzrouter
	}
	return json.Marshal(resp)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// mapFinishToStatus collapses a chat finish_reason to the Responses
// (status, incomplete_details.reason) tuple. The second return is the
// "reason" sub-code attached when status == "incomplete" so SDK clients
// can branch (max_output_tokens vs. content_filter), and is empty for
// successful completions.
func mapFinishToStatus(reason string) (string, string) {
	switch reason {
	case "stop", "tool_calls", "function_call", "":
		return "completed", ""
	case "length":
		return "incomplete", "max_output_tokens"
	case "content_filter":
		return "incomplete", "content_filter"
	default:
		return "completed", ""
	}
}

// translateUsage maps Chat's {prompt_tokens, completion_tokens,
// total_tokens, prompt_tokens_details{cached_tokens}} to Responses'
// {input_tokens, output_tokens, total_tokens,
// input_tokens_details{cached_tokens}}.
//
// LiteLLM bug #22192: cached_tokens must survive into the Responses
// usage block on streaming response.completed events. The translator
// extracts the cached_tokens count when present and re-emits it under
// input_tokens_details.cached_tokens.
// extractZzFromUsage pulls the streaming zz_* namespace (provider, node,
// cost_source, cost_usd, latency_ms, ttft_ms, tokens_per_second) out of
// a chat usage block and returns a top-level zzrouter map for the
// /v1/responses envelope. Returns nil when no zz_* fields are present.
func extractZzFromUsage(raw json.RawMessage) map[string]any {
	var bag map[string]any
	if err := json.Unmarshal(raw, &bag); err != nil || bag == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range bag {
		if !strings.HasPrefix(k, "zz_") {
			continue
		}
		out[strings.TrimPrefix(k, "zz_")] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// translateUsage extracts only the canonical token-count fields from
// a chat usage block into the Responses-shape usage envelope.
//
// LOAD-BEARING: zz_* fields in the input are intentionally dropped here
// — extractZzFromUsage lifts them into the top-level zzrouter block on
// response.completed / response.failed, so duplicating them inside
// usage would re-create the double-inject contract violation that the
// non-streaming chat path was just cleaned up to remove. If a future
// patch adds passthrough of unknown usage fields, gate it on a
// blocklist that excludes the zz_ prefix.
func translateUsage(raw json.RawMessage) map[string]any {
	var u struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil
	}
	out := map[string]any{
		"input_tokens":  u.PromptTokens,
		"output_tokens": u.CompletionTokens,
		"total_tokens":  u.TotalTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.PromptTokensDetails.CachedTokens,
		},
	}
	return out
}

// ---------- SSE streaming translator ----------

// responsesStreamWriter wraps gin.ResponseWriter to translate a stream
// of chat.completion.chunk SSE frames into the response.* event
// sequence. It buffers partial frames between Write calls, parses each
// complete `data: ...\n\n` block, and emits one or more
// translated events to the underlying writer.
//
// State machine (single output_item / single content_part for plain
// text streams):
//
//	response.created     (on first Write)
//	response.in_progress (on first Write)
//	response.output_item.added       (on first content delta)
//	response.content_part.added      (on first content delta)
//	response.output_text.delta       (per content delta)
//	response.content_part.done       (on finish_reason)
//	response.output_item.done        (on finish_reason)
//	response.completed               (on finish_reason; carries usage)
//
// For tool-call deltas, a separate function_call output_item is
// emitted with response.function_call_arguments.delta events
// accumulating the arguments string.
type responsesStreamWriter struct {
	gin.ResponseWriter
	model           string
	respID          string
	createdAt       int64
	headerWritten   bool
	openedStream    bool
	textItemOpen    bool
	textItemIdx     int
	contentPartOpen bool
	toolItems       map[int]*toolItemState // chat tool_call index → emitted item
	nextOutputIdx   int
	sequence        int
	buf             bytes.Buffer
	// finalUsage is captured from the last chunk that carries usage so
	// response.completed can re-emit it.
	finalUsage json.RawMessage
	// finishReason is captured from choices[0].finish_reason when set.
	finishReason string
	// completedSent guards against double-emission of response.completed
	// when the upstream sends [DONE] AND the http handler returns.
	completedSent bool
}

type toolItemState struct {
	itemID       string
	callID       string
	name         string
	outputIdx    int
	argsBuffered string
	emittedAdded bool
}

func newResponsesStreamWriter(w gin.ResponseWriter, model string) *responsesStreamWriter {
	respID := "resp_" + strconv.FormatInt(utils.Now().UnixNano(), 36)
	return &responsesStreamWriter{
		ResponseWriter: w,
		model:          model,
		respID:         respID,
		createdAt:      utils.Now().Unix(),
		toolItems:      map[int]*toolItemState{},
	}
}

func (w *responsesStreamWriter) WriteHeader(code int) {
	if w.headerWritten {
		return
	}
	w.headerWritten = true
	if code >= http.StatusBadRequest {
		// A pre-stream failure remains a JSON HTTP error, even if streaming was requested.
		w.ResponseWriter.WriteHeader(code)
		return
	}
	// Replace upstream Content-Type — we are emitting Responses events
	// regardless of what the chat backend declared.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The translated body is not the one the backend measured.
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
}

func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	if w.ResponseWriter.Status() >= http.StatusBadRequest {
		return w.ResponseWriter.Write(p)
	}
	if !w.openedStream {
		w.openedStream = true
		w.emitEvent("response.created", w.envelopeEvent("in_progress"))
		w.emitEvent("response.in_progress", w.envelopeEvent("in_progress"))
	}
	if w.buf.Len()+len(p) > maxStreamBufBytes {
		// Upstream is streaming without frame terminators; abort.
		// Emit response.failed and stop translating. We still ack the
		// write to keep the upstream from blocking on the writer.
		w.emitFailure("upstream_stream_buffer_overflow")
		w.buf.Reset()
		return len(p), nil
	}
	w.buf.Write(p)
	w.drainFrames()
	return len(p), nil
}

// emitFailure emits a terminal response.failed event with a stable
// machine-readable code. Idempotent via completedSent.
func (w *responsesStreamWriter) emitFailure(code string) {
	if w.completedSent {
		return
	}
	w.completedSent = true
	failed := w.envelopeEvent("failed")
	setEnvelopeField(failed, "error", map[string]any{
		"code":    code,
		"message": "translation shim aborted: " + code,
	})
	if zz := extractZzFromUsage(w.finalUsage); zz != nil {
		setEnvelopeField(failed, "zzrouter", zz)
	}
	w.emitEvent("response.failed", failed)
	_, _ = w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// emitUpstreamError translates a chat-side mid-stream error frame into
// a terminal response.failed event, preserving upstream's type/code/
// message verbatim. Idempotent via completedSent.
//
// Deliberate: an open content_part / output_item is NOT closed before
// response.failed. response.failed is terminal per OpenAI Responses
// semantics; an aborted stream does not produce matching .done events
// for .added events. finalize() honours this by early-returning on
// completedSent.
func (w *responsesStreamWriter) emitUpstreamError(raw json.RawMessage) {
	if w.completedSent {
		return
	}
	w.completedSent = true
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		obj = map[string]any{
			"code":    "upstream_error",
			"message": "upstream emitted an unparseable error frame",
		}
	}
	failed := w.envelopeEvent("failed")
	setEnvelopeField(failed, "error", obj)
	if zz := extractZzFromUsage(w.finalUsage); zz != nil {
		setEnvelopeField(failed, "zzrouter", zz)
	}
	w.emitEvent("response.failed", failed)
	_, _ = w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// drainFrames pulls complete `\n\n`-terminated SSE frames out of the
// buffer and translates each.
func (w *responsesStreamWriter) drainFrames() {
	for {
		idx := bytes.Index(w.buf.Bytes(), []byte("\n\n"))
		if idx < 0 {
			return
		}
		frame := w.buf.Next(idx + 2)
		w.handleFrame(frame)
	}
}

func (w *responsesStreamWriter) handleFrame(frame []byte) {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			w.finalize()
			return
		}
		w.handleChunk(payload)
	}
}

type chatChunk struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Error   json.RawMessage `json:"error,omitempty"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role,omitempty"`
			Content   string `json:"content,omitempty"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func (w *responsesStreamWriter) handleChunk(payload []byte) {
	var chunk chatChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	// Explicit `"error": null` is load-bearing: some providers clear the
	// field per chunk; without the null guard those chunks would falsely
	// trigger response.failed.
	if len(chunk.Error) > 0 && !bytes.Equal(bytes.TrimSpace(chunk.Error), []byte("null")) {
		w.emitUpstreamError(chunk.Error)
		return
	}
	if len(chunk.Usage) > 0 {
		w.finalUsage = append(w.finalUsage[:0], chunk.Usage...)
	}
	if chunk.Model != "" {
		w.model = chunk.Model
	}

	for _, ch := range chunk.Choices {
		if ch.Delta.Content != "" {
			w.handleContentDelta(ch.Delta.Content)
		}
		for _, tc := range ch.Delta.ToolCalls {
			w.handleToolCallDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			w.finishReason = *ch.FinishReason
		}
	}
}

func (w *responsesStreamWriter) handleContentDelta(delta string) {
	if !w.textItemOpen {
		w.textItemIdx = w.nextOutputIdx
		w.nextOutputIdx++
		w.textItemOpen = true
		itemID := "msg_" + w.respID
		w.emitEvent("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": w.nextSeq(),
			"output_index":    w.textItemIdx,
			"item": map[string]any{
				"id":      itemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		w.contentPartOpen = true
		w.emitEvent("response.content_part.added", map[string]any{
			"type":            "response.content_part.added",
			"sequence_number": w.nextSeq(),
			"item_id":         itemID,
			"output_index":    w.textItemIdx,
			"content_index":   0,
			"part": map[string]any{
				"type":        "output_text",
				"text":        "",
				"annotations": []any{},
			},
		})
	}
	w.emitEvent("response.output_text.delta", map[string]any{
		"type":            "response.output_text.delta",
		"sequence_number": w.nextSeq(),
		"item_id":         "msg_" + w.respID,
		"output_index":    w.textItemIdx,
		"content_index":   0,
		"delta":           delta,
	})
}

func (w *responsesStreamWriter) handleToolCallDelta(idx int, id, name, args string) {
	st, ok := w.toolItems[idx]
	if !ok {
		st = &toolItemState{
			outputIdx: w.nextOutputIdx,
		}
		w.nextOutputIdx++
		w.toolItems[idx] = st
	}
	if id != "" {
		st.callID = id
		st.itemID = "fc_" + id
	}
	if name != "" {
		st.name = name
	}
	// Defer "added" until we have at least an id or name so the event
	// carries non-empty identity.
	if !st.emittedAdded && (st.callID != "" || st.name != "") {
		st.emittedAdded = true
		if st.itemID == "" {
			st.itemID = "fc_" + strconv.Itoa(idx) + "_" + w.respID
		}
		w.emitEvent("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": w.nextSeq(),
			"output_index":    st.outputIdx,
			"item": map[string]any{
				"id":        st.itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   st.callID,
				"name":      st.name,
				"arguments": "",
			},
		})
	}
	if args != "" && st.emittedAdded {
		st.argsBuffered += args
		w.emitEvent("response.function_call_arguments.delta", map[string]any{
			"type":            "response.function_call_arguments.delta",
			"sequence_number": w.nextSeq(),
			"item_id":         st.itemID,
			"output_index":    st.outputIdx,
			"delta":           args,
		})
	}
}

func (w *responsesStreamWriter) finalize() {
	// If a terminal event (response.failed) already fired, suppress all
	// further finalization. No content_part.done / output_item.done /
	// response.completed may follow a response.failed.
	if w.completedSent {
		return
	}
	// Close text item if open.
	if w.textItemOpen {
		itemID := "msg_" + w.respID
		if w.contentPartOpen {
			w.emitEvent("response.content_part.done", map[string]any{
				"type":            "response.content_part.done",
				"sequence_number": w.nextSeq(),
				"item_id":         itemID,
				"output_index":    w.textItemIdx,
				"content_index":   0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
			w.contentPartOpen = false
		}
		w.emitEvent("response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": w.nextSeq(),
			"output_index":    w.textItemIdx,
			"item": map[string]any{
				"id":     itemID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
			},
		})
		w.textItemOpen = false
	}
	// Close tool items in monotonic output_index order. Map iteration is
	// randomized, and chat-side tool_call indices may not match arrival
	// order; finalize must emit done events with output_index ascending
	// so a client building output[] sees a clean sequence.
	ordered := make([]*toolItemState, 0, len(w.toolItems))
	for _, st := range w.toolItems {
		ordered = append(ordered, st)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].outputIdx < ordered[j].outputIdx })
	for _, st := range ordered {
		if !st.emittedAdded {
			continue
		}
		w.emitEvent("response.function_call_arguments.done", map[string]any{
			"type":            "response.function_call_arguments.done",
			"sequence_number": w.nextSeq(),
			"item_id":         st.itemID,
			"output_index":    st.outputIdx,
			"arguments":       st.argsBuffered,
		})
		w.emitEvent("response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": w.nextSeq(),
			"output_index":    st.outputIdx,
			"item": map[string]any{
				"id":        st.itemID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   st.callID,
				"name":      st.name,
				"arguments": st.argsBuffered,
			},
		})
	}
	if w.completedSent {
		return
	}
	w.completedSent = true
	completed := w.envelopeEvent("completed")
	if len(w.finalUsage) > 0 {
		setEnvelopeField(completed, "usage", translateUsage(w.finalUsage))
		// The streaming chat path injects zz_* fields into the terminal
		// usage chunk; lift them into a top-level zzrouter block on the
		// completed envelope so /v1/responses streaming consumers see
		// the same routing/cost provenance as the non-streaming
		// translator's pass-through of env.Zzrouter.
		if zz := extractZzFromUsage(w.finalUsage); zz != nil {
			setEnvelopeField(completed, "zzrouter", zz)
		}
	}
	w.emitEvent("response.completed", completed)
	// Emit terminal SSE [DONE] sentinel for clients that expect it.
	_, _ = w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
	flusher, _ := w.ResponseWriter.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
}

// setEnvelopeField attaches a key on the inner "response" object of a
// response.* event. No-op (and silently safe) on payloads whose inner
// shape was changed by a future refactor — guards against the runtime
// panic that an unchecked map[string]any type assertion would cause
// mid-stream.
func setEnvelopeField(payload map[string]any, key string, value any) {
	inner, ok := payload["response"].(map[string]any)
	if !ok {
		return
	}
	inner[key] = value
}

func (w *responsesStreamWriter) envelopeEvent(status string) map[string]any {
	return map[string]any{
		"type":            "response." + status,
		"sequence_number": w.nextSeq(),
		"response": map[string]any{
			"id":         w.respID,
			"object":     "response",
			"created_at": w.createdAt,
			"model":      w.model,
			"status":     status,
			"output":     []any{},
		},
	}
}

func (w *responsesStreamWriter) nextSeq() int {
	s := w.sequence
	w.sequence++
	return s
}

func (w *responsesStreamWriter) emitEvent(name string, payload map[string]any) {
	// `name` is the canonical event type. envelopeEvent prefills `type`
	// from its status arg (which is correct for completed/failed where
	// event type matches status, but wrong for response.created whose
	// inner status is in_progress). Always overwrite from name so the
	// SSE `event:` header and the inner `type` field can never drift.
	payload["type"] = name
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = w.ResponseWriter.Write([]byte("event: "))
	_, _ = w.ResponseWriter.Write([]byte(name))
	_, _ = w.ResponseWriter.Write([]byte("\ndata: "))
	_, _ = w.ResponseWriter.Write(body)
	_, _ = w.ResponseWriter.Write([]byte("\n\n"))
	flusher, _ := w.ResponseWriter.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
}

// Close flushes any buffered SSE frame and emits the terminal
// response.completed event when the upstream forgot to send `[DONE]`.
// Idempotent.
func (w *responsesStreamWriter) Close() {
	if w.buf.Len() > 0 {
		w.buf.Write([]byte("\n\n"))
		w.drainFrames()
	}
	if w.openedStream && !w.completedSent {
		w.finalize()
	}
}

// ---------- Non-streaming buffered writer ----------

// responsesBufferedWriter captures a non-streaming chat completion
// JSON response, then translates and writes the Responses envelope on
// Finalize. Suppresses upstream writes until Finalize so the upstream
// writer never sees the chat-shaped body.
//
// gin.ResponseWriter is a superset of http.ResponseWriter with
// Status/Size/Written/Flush/WriteString/WriteHeaderNow. Middleware
// commonly keys behaviour off Written/Status, so we override those to
// reflect our buffered state rather than the underlying (untouched)
// writer's state.
type responsesBufferedWriter struct {
	gin.ResponseWriter
	model     string
	respID    string
	status    int
	header    http.Header
	body      bytes.Buffer
	wrote     bool
	finalDone bool
}

func newResponsesBufferedWriter(w gin.ResponseWriter, model string) *responsesBufferedWriter {
	return &responsesBufferedWriter{
		ResponseWriter: w,
		model:          model,
		header:         http.Header{},
		status:         http.StatusOK,
	}
}

func (w *responsesBufferedWriter) Header() http.Header {
	return w.header
}

func (w *responsesBufferedWriter) WriteHeader(code int) {
	w.status = code
}

func (w *responsesBufferedWriter) Write(p []byte) (int, error) {
	w.wrote = true
	w.body.Write(p)
	return len(p), nil
}

func (w *responsesBufferedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

// Status reports the captured upstream status (defaults 200).
func (w *responsesBufferedWriter) Status() int { return w.status }

// Size reports the buffered body length (gin uses this for access logs).
func (w *responsesBufferedWriter) Size() int { return w.body.Len() }

// Written reports whether anything has been buffered yet.
func (w *responsesBufferedWriter) Written() bool { return w.wrote }

// WriteHeaderNow is a no-op; we delay headers until Finalize.
func (w *responsesBufferedWriter) WriteHeaderNow() {}

// Flush is a no-op; buffering would be defeated otherwise.
func (w *responsesBufferedWriter) Flush() {}

// Finalize translates the buffered chat body and writes the response.
// On non-2xx upstream status, the body is forwarded as-is (errors
// already carry an OpenAI-shaped envelope which is appropriate to
// surface to the caller).
func (w *responsesBufferedWriter) Finalize() {
	if w.finalDone {
		return
	}
	w.finalDone = true

	if w.status < 200 || w.status >= 300 {
		w.flushHeaders(w.header.Get("Content-Type"))
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write(w.body.Bytes())
		return
	}
	out, err := translateChatToResponses(w.body.Bytes(), w.model)
	if err != nil {
		w.flushHeaders("application/json")
		w.ResponseWriter.WriteHeader(http.StatusBadGateway)
		_, _ = w.ResponseWriter.Write([]byte(`{"error":{"type":"api_error","code":"upstream_chat_envelope_unparseable","message":"upstream chat envelope did not parse"}}`))
		return
	}
	// Capture the translated response id so handleResponsesViaTranslation
	// can record affinity without re-translating.
	var peek struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(out, &peek)
	w.respID = peek.ID
	w.flushHeaders("application/json")
	w.ResponseWriter.WriteHeader(http.StatusOK)
	_, _ = w.ResponseWriter.Write(out)
}

func (w *responsesBufferedWriter) flushHeaders(contentType string) {
	for k, vs := range w.header {
		// Skip Content-Length — translated body has a different size.
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Type") {
			continue
		}
		for _, v := range vs {
			w.ResponseWriter.Header().Add(k, v)
		}
	}
	if contentType != "" {
		w.ResponseWriter.Header().Set("Content-Type", contentType)
	}
}
