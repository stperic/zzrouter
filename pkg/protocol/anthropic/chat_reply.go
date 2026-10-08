package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A Chat Completions reply as the compat translation reads it.
type chatReply struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type chatMessage struct {
	Content json.RawMessage `json:"content"`
	// Engines name a reasoning model's thinking either way.
	ReasoningContent string         `json:"reasoning_content"`
	Reasoning        string         `json:"reasoning"`
	ToolCalls        []chatToolCall `json:"tool_calls"`
}

type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// Usage is a Messages reply's token accounting. Anthropic counts cache
// reads apart from input_tokens.
type Usage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens,omitempty"`
}

func (u *chatUsage) messagesUsage() Usage {
	if u == nil {
		return Usage{}
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	// An engine can report more cached than prompt tokens; a count is
	// never negative.
	return Usage{InputTokens: max(0, u.PromptTokens-cached), OutputTokens: u.CompletionTokens, CacheReadInputTokens: cached}
}

// Stop reasons a Messages reply carries.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopToolUse   = "tool_use"
	stopRefusal   = "refusal"
)

// stopReason maps a chat finish_reason; anything else ended the turn.
func stopReason(finish string) string {
	switch finish {
	case "length":
		return stopMaxTokens
	case "tool_calls", "function_call":
		return stopToolUse
	case "content_filter":
		return stopRefusal
	default:
		return stopEndTurn
	}
}

// stopWithTools is the stop reason of a turn that ended with tool calls:
// tool_use, whatever the engine called it, since that is what tells an
// Anthropic client to run them.
func stopWithTools(stop string, hasTools bool) string {
	if hasTools && stop == stopEndTurn {
		return stopToolUse
	}
	return stop
}

// messageIDPrefix is how a Messages reply id starts.
const messageIDPrefix = "msg_"

// anonymousMessageID stands in when the engine gave its reply no id.
const anonymousMessageID = messageIDPrefix + "zzrouter"

func messageID(chatID string) string {
	switch {
	case chatID == "":
		return anonymousMessageID
	case strings.HasPrefix(chatID, messageIDPrefix):
		return chatID
	default:
		return messageIDPrefix + chatID
	}
}

// toolUseIDPrefix names a tool call the engine left without an id.
const toolUseIDPrefix = "toolu_"

func toolUseID(id string, index int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("%s%d", toolUseIDPrefix, index)
}

// toolInput parses a call's arguments. Arguments that are not a JSON
// object become an empty input rather than failing the whole turn; the
// client's tool then says what is missing.
func toolInput(arguments string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &obj) != nil || obj == nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

// FromChatReply translates a Chat Completions reply into a Messages
// reply, answering as model (the name the client used).
func FromChatReply(body []byte, model string) ([]byte, error) {
	var reply chatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("engine reply is not Chat Completions: %w", err)
	}
	if len(reply.Choices) == 0 {
		return nil, fmt.Errorf("engine reply has no choices")
	}
	choice := reply.Choices[0]
	content := []map[string]any{}
	if thinking := firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning); thinking != "" {
		content = append(content, map[string]any{"type": blockThinking, "thinking": thinking, "signature": ""})
	}
	if text := contentText(choice.Message.Content); text != "" {
		content = append(content, map[string]any{"type": blockText, "text": text})
	}
	for i, call := range choice.Message.ToolCalls {
		content = append(content, map[string]any{
			"type": blockToolUse, "id": toolUseID(call.ID, i), "name": call.Function.Name,
			"input": toolInput(call.Function.Arguments),
		})
	}
	return json.Marshal(map[string]any{
		"id": messageID(reply.ID), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopWithTools(stopReason(choice.FinishReason), len(choice.Message.ToolCalls) > 0), "stop_sequence": nil,
		"usage": reply.Usage.messagesUsage(),
	})
}

// contentText reads chat content as a string or a list of text parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// FromChatTokenCount answers count_tokens from the prompt usage of the
// one-token completion ToCountTokensRequest asked for. The count is the
// whole prompt, cached or not, as Anthropic's is.
func FromChatTokenCount(body []byte, _ string) ([]byte, error) {
	var reply chatReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("engine reply is not Chat Completions: %w", err)
	}
	if reply.Usage == nil {
		return nil, fmt.Errorf("engine reply reports no usage to count from")
	}
	return json.Marshal(map[string]any{"input_tokens": reply.Usage.PromptTokens})
}
