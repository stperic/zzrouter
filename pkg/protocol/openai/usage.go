package openai

// NormalizeChatUsage removes vendor accounting only at known chat usage paths.
func NormalizeChatUsage(obj map[string]any) bool {
	if _, chat := obj["choices"].([]any); !chat {
		return false
	}
	if kind, _ := obj["object"].(string); kind != "" && kind != "chat.completion" && kind != "chat.completion.chunk" {
		return false
	}
	usage, _ := obj["usage"].(map[string]any)
	changed := false
	for _, key := range []string{"cost", "cost_details", "is_byok"} {
		if _, exists := usage[key]; exists {
			delete(usage, key)
			changed = true
		}
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if _, exists := details["cache_write_tokens"]; exists {
			delete(details, "cache_write_tokens")
			changed = true
		}
	}
	return changed
}
