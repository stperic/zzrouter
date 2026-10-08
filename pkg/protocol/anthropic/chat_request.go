package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A Messages request as the compat translation reads it. Fields it does
// not read (cache_control, thinking, service_tier, ...) are not carried:
// Chat Completions has no place for them.
type messagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []message       `json:"messages"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Tools         []tool          `json:"tools"`
	ToolChoice    *toolChoice     `json:"tool_choice"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// block is any content block; Type says which fields it uses.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *imageSource    `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
}

// Content block types the translation knows.
const (
	blockText             = "text"
	blockImage            = "image"
	blockToolUse          = "tool_use"
	blockToolResult       = "tool_result"
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
)

// textBlockSeparator joins a turn's text blocks into one chat content
// string: every engine takes a string, not all take a list of parts.
const textBlockSeparator = "\n\n"

// toolErrorPrefix marks a failed tool's result, which Chat Completions
// has no flag for, so the model can still tell.
const toolErrorPrefix = "Error: "

// TranslateError is a Messages request the translation refuses: the
// Anthropic dialect's 400, naming the parameter.
type TranslateError struct {
	Param   string
	Message string
}

func (e *TranslateError) Error() string { return e.Param + ": " + e.Message }

func refuse(param, format string, args ...any) *TranslateError {
	return &TranslateError{Param: param, Message: fmt.Sprintf(format, args...)}
}

// ChatRequest is a Messages request rendered as Chat Completions, for a
// provider whose engine speaks only that (messages_compat).
type ChatRequest struct {
	Body   []byte
	Model  string
	Stream bool
}

// ToChatRequest translates a Messages request body into a Chat
// Completions body. See docs/plan_messages_compat.md for the mapping.
func ToChatRequest(body []byte) (ChatRequest, error) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ChatRequest{}, refuse("body", "not a Messages request: %v", err)
	}
	messages, err := chatMessages(req)
	if err != nil {
		return ChatRequest{}, err
	}
	chat := map[string]any{"model": req.Model, "messages": messages}
	if req.MaxTokens != nil {
		chat["max_tokens"] = *req.MaxTokens
	}
	if len(req.StopSequences) > 0 {
		chat["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		chat["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chat["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		chat["top_k"] = *req.TopK
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		chat["user"] = req.Metadata.UserID
	}
	if tools := chatTools(req.Tools); len(tools) > 0 {
		chat["tools"] = tools
		if req.ToolChoice != nil {
			if choice := chatToolChoice(*req.ToolChoice, tools); choice != nil {
				chat["tool_choice"] = choice
			}
			if req.ToolChoice.DisableParallelToolUse {
				chat["parallel_tool_calls"] = false
			}
		}
	}
	if req.Stream {
		chat["stream"] = true
		// The reply's usage comes from this frame; without it a streamed
		// turn would settle no tokens.
		chat["stream_options"] = map[string]any{"include_usage": true}
	}
	out, mErr := json.Marshal(chat)
	if mErr != nil {
		return ChatRequest{}, refuse("body", "cannot encode the translated request: %v", mErr)
	}
	return ChatRequest{Body: out, Model: req.Model, Stream: req.Stream}, nil
}

// chatMessages renders the system prompt and every turn.
func chatMessages(req messagesRequest) ([]map[string]any, error) {
	var out []map[string]any
	system, err := systemText(req.System)
	if err != nil {
		return nil, err
	}
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for i, m := range req.Messages {
		blocks, err := contentBlocks(m.Content, fmt.Sprintf("messages.%d.content", i))
		if err != nil {
			return nil, err
		}
		var turn []map[string]any
		switch {
		case m.Role == "user":
			turn, err = userTurn(blocks, i)
		case m.Role == "assistant":
			turn, err = assistantTurn(blocks, i)
		case contextRoles[m.Role]:
			turn, err = contextTurn(blocks, i)
		default:
			err = refuse(fmt.Sprintf("messages.%d.role", i), "must be user or assistant, not %q", m.Role)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, turn...)
	}
	return out, nil
}

// contextRoles are the roles Claude Code (2.1.154 on) puts in messages
// besides user and assistant, carrying the harness's context mid-
// conversation. Chat Completions takes a system message anywhere, so
// each becomes one; a chat template that cannot render that is the
// model default's to replace.
var contextRoles = map[string]bool{"system": true, "ctx": true, "msg": true}

// contextTurn renders a context-role turn as a system message.
func contextTurn(blocks []block, idx int) ([]map[string]any, error) {
	var texts []string
	for j, b := range blocks {
		if b.Type != blockText {
			return nil, refuse(fmt.Sprintf("messages.%d.content.%d", idx, j), "a context message carries text, not %q", b.Type)
		}
		texts = append(texts, b.Text)
	}
	return []map[string]any{{"role": "system", "content": strings.Join(texts, textBlockSeparator)}}, nil
}

// systemText reads system as a string or as text blocks.
func systemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	blocks, err := contentBlocks(raw, "system")
	if err != nil {
		return "", err
	}
	var texts []string
	for _, b := range blocks {
		if b.Type != blockText {
			return "", refuse("system", "only text blocks can be a system prompt, not %q", b.Type)
		}
		texts = append(texts, b.Text)
	}
	return strings.Join(texts, textBlockSeparator), nil
}

// contentBlocks reads content as a string (one text block) or a list of
// blocks.
func contentBlocks(raw json.RawMessage, param string) ([]block, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: blockText, Text: s}}, nil
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, refuse(param, "must be a string or a list of content blocks")
	}
	return blocks, nil
}

// userTurn renders a user turn: each tool result becomes a tool message,
// ahead of the rest of the turn, which is where Chat Completions expects
// a tool's answer — right after the call.
func userTurn(blocks []block, idx int) ([]map[string]any, error) {
	var out []map[string]any
	var texts []string
	var parts []map[string]any
	hasImage := false
	for j, b := range blocks {
		param := fmt.Sprintf("messages.%d.content.%d", idx, j)
		switch b.Type {
		case blockText:
			texts = append(texts, b.Text)
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case blockImage:
			url, err := imageURL(b.Source, param)
			if err != nil {
				return nil, err
			}
			hasImage = true
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case blockToolResult:
			text, images, err := toolResultContent(b, param)
			if err != nil {
				return nil, err
			}
			if len(images) > 0 {
				hasImage = true
				parts = append(parts, images...)
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
		default:
			return nil, refuse(param, "a user turn cannot carry a %q block on this model's engine", b.Type)
		}
	}
	switch {
	case hasImage:
		out = append(out, map[string]any{"role": "user", "content": parts})
	case len(texts) > 0:
		out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, textBlockSeparator)})
	}
	return out, nil
}

// assistantTurn renders an assistant turn: its text, and its tool calls.
// Thinking is dropped: a chat template renders reasoning from the model's
// own output, and Anthropic itself drops earlier turns' thinking.
func assistantTurn(blocks []block, idx int) ([]map[string]any, error) {
	var texts []string
	var calls []map[string]any
	for j, b := range blocks {
		switch b.Type {
		case blockText:
			texts = append(texts, b.Text)
		case blockToolUse:
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		case blockThinking, blockRedactedThinking:
		default:
			return nil, refuse(fmt.Sprintf("messages.%d.content.%d", idx, j),
				"an assistant turn cannot carry a %q block on this model's engine", b.Type)
		}
	}
	turn := map[string]any{"role": "assistant", "content": strings.Join(texts, textBlockSeparator)}
	if len(calls) > 0 {
		turn["tool_calls"] = calls
	}
	return []map[string]any{turn}, nil
}

// imageURL renders an image source as the URL Chat Completions takes: a
// data URL for inline bytes.
func imageURL(src *imageSource, param string) (string, error) {
	switch {
	case src == nil:
		return "", refuse(param+".source", "an image needs a source")
	case src.Type == "base64":
		return "data:" + src.MediaType + ";base64," + src.Data, nil
	case src.Type == "url":
		return src.URL, nil
	default:
		return "", refuse(param+".source.type", "must be base64 or url, not %q", src.Type)
	}
}

// toolResultContent preserves tool text in its tool reply and lifts images
// into the following user turn, since Chat tool messages cannot carry images.
func toolResultContent(b block, param string) (string, []map[string]any, error) {
	var text string
	var images []map[string]any
	if len(b.Content) > 0 && string(b.Content) != "null" {
		blocks, err := contentBlocks(b.Content, param+".content")
		if err != nil {
			return "", nil, err
		}
		var texts []string
		for i, c := range blocks {
			switch c.Type {
			case blockText:
				texts = append(texts, c.Text)
			case blockImage:
				url, err := imageURL(c.Source, fmt.Sprintf("%s.content.%d", param, i))
				if err != nil {
					return "", nil, err
				}
				images = append(images, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			default:
				return "", nil, refuse(param+".content", "a tool result cannot carry a %q block on this model's engine", c.Type)
			}
		}
		text = strings.Join(texts, textBlockSeparator)
	}
	if b.IsError {
		text = toolErrorPrefix + text
	}
	return text, images, nil
}

// chatTools renders client tools as functions. A server tool (a typed
// tool with no input schema, such as web search) has no Chat Completions
// form and is left out; the engine cannot run it.
func chatTools(tools []tool) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		if len(t.InputSchema) == 0 {
			continue
		}
		fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// chatToolChoice maps tool_choice; an unknown type, or a named tool that
// was left out, leaves the engine's default (auto).
func chatToolChoice(c toolChoice, tools []map[string]any) any {
	switch c.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		for _, t := range tools {
			fn, ok := t["function"].(map[string]any)
			if ok && fn["name"] == c.Name {
				return map[string]any{"type": "function", "function": map[string]any{"name": c.Name}}
			}
		}
		return nil
	default:
		return nil
	}
}

// countTokensMaxTokens is the completion a token count spends: the
// prompt is what is counted, so one token is enough to get its usage.
const countTokensMaxTokens = 1

// ToCountTokensRequest renders a count_tokens body as the Chat
// Completions request whose prompt usage answers it: the same
// translation, generating one token, not streamed. An engine without
// the Messages API has no count endpoint, and the tokenizer endpoints
// engines do have differ in shape, so prompt usage is the one count
// every engine reports.
func ToCountTokensRequest(body []byte) (ChatRequest, error) {
	req, err := ToChatRequest(body)
	if err != nil {
		return ChatRequest{}, err
	}
	var chat map[string]any
	if err := json.Unmarshal(req.Body, &chat); err != nil {
		return ChatRequest{}, refuse("body", "cannot re-read the translated request: %v", err)
	}
	chat["max_tokens"] = countTokensMaxTokens
	delete(chat, "stream")
	delete(chat, "stream_options")
	out, mErr := json.Marshal(chat)
	if mErr != nil {
		return ChatRequest{}, refuse("body", "cannot encode the translated request: %v", mErr)
	}
	return ChatRequest{Body: out, Model: req.Model}, nil
}
