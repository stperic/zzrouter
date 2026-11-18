package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ChatExtensionError identifies an extra_params field that cannot be merged safely.
type ChatExtensionError struct {
	Param   string
	Message string
}

func (e *ChatExtensionError) Error() string { return e.Param + ": " + e.Message }

// MergeChatExtensions carries native extra_params into a translated Chat request.
// Reconstructed controls stay owned by the source protocol, even when omitted.
func MergeChatExtensions(chat map[string]any, raw json.RawMessage) error {
	for key, value := range chat {
		if err := validateChatOutputOption(key, key, value); err != nil {
			return err
		}
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return &ChatExtensionError{Param: "extra_params", Message: "must be an object of native Chat Completions options"}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		_, exists := chat[key]
		switch strings.ToLower(key) {
		case "model", "messages", "tools", "tool_choice", "parallel_tool_calls", "stream", "stream_options",
			"max_tokens", "max_completion_tokens", "temperature", "top_p", "response_format", "user", "reasoning_effort":
			exists = true
		}
		if exists {
			return &ChatExtensionError{Param: "extra_params." + key, Message: fmt.Sprintf("cannot override translated field %q; set it in the source request instead", key)}
		}
		if err := validateChatOutputOption(key, "extra_params."+key, fields[key]); err != nil {
			return err
		}
	}
	for key, value := range fields {
		chat[key] = value
	}
	return nil
}

// A shim's single text/tool reply cannot represent native alternate or audio outputs.
func validateChatOutputOption(key, param string, value any) error {
	key = strings.ToLower(key)
	switch key {
	case "n", "logprobs", "top_logprobs", "modalities", "audio":
	default:
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return &ChatExtensionError{Param: param, Message: "cannot encode native output option"}
	}
	if string(raw) == "null" {
		return nil
	}
	switch key {
	case "n":
		var n int
		if json.Unmarshal(raw, &n) == nil && n == 1 {
			return nil
		}
	case "logprobs":
		if string(raw) == "false" {
			return nil
		}
	case "top_logprobs":
		if string(raw) == "0" {
			return nil
		}
	case "modalities":
		var modalities []string
		if json.Unmarshal(raw, &modalities) == nil && len(modalities) == 1 && modalities[0] == "text" {
			return nil
		}
	case "audio":
	default:
		return nil
	}
	return &ChatExtensionError{Param: param, Message: "requested output cannot be represented by the translation shim; use a native endpoint"}
}
