package normalizer

// RegisterMLX keeps reasoning models readable by clients that consume only content.
func RegisterMLX(r *Registry) {
	r.Register("mlx", normalizeMLX)
}

func normalizeMLX(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	changed := false
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		for _, field := range []string{"message", "delta"} {
			if msg, ok := choice[field].(map[string]any); ok {
				changed = copyReasoningIntoContent(msg) || changed
			}
		}
	}
	return changed
}

func copyReasoningIntoContent(msg map[string]any) bool {
	reasoning, ok := msg["reasoning"].(string)
	if !ok || reasoning == "" {
		return false
	}
	if existing, ok := msg["content"].(string); ok && existing != "" {
		return false
	}
	msg["content"] = reasoning
	return true
}
