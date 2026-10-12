package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// compactModelShow keeps reported evidence below the MCP response limit without altering raw detail.
func compactModelShow(raw map[string]any) (map[string]any, error) {
	const compactDetailLimit = 64 << 10
	out := make(map[string]any)
	for _, key := range []string{"name", "node", "assigned_app", "source_repo", "source_id", "parameters", "capabilities", "template_present"} {
		if value, exists := raw[key]; exists {
			out[key] = value
		}
	}
	if template, reported := raw["template"].(string); reported {
		out["template_present"] = strings.TrimSpace(template) != ""
	}
	if info, ok := raw["model_info"].(map[string]any); ok {
		context := make(map[string]any)
		for key, value := range info {
			if key == "max_position_embeddings" || key == "context_length" || key == "sliding_window" || strings.HasSuffix(key, ".context_length") {
				if _, numeric := value.(float64); numeric {
					context[key] = value
				}
			}
		}
		if len(context) > 0 {
			out["model_info"] = context
		}
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > compactDetailLimit {
		return nil, newRouteProblemError(http.StatusBadGateway, "Bad Gateway", "compact model detail exceeds its bounded JSON contract", httperr.CodeUpstreamShape, stringFromMaps("node", raw))
	}
	return out, nil
}
