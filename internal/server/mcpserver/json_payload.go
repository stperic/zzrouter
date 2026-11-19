package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// Raw JSON values retain nested provider data and numbers through SDK and REST round trips.
type jsonOutput map[string]json.RawMessage

func (out jsonOutput) summary() string {
	body, err := json.Marshal(out)
	if err != nil {
		return "API response could not be encoded as JSON."
	}
	return string(body)
}

func jsonCall(d dispatcher, ctx context.Context, req request) (jsonOutput, error) {
	var out jsonOutput
	err := d.call(ctx, req, &out)
	return out, err
}

func inferenceJSON(common map[string]any, options map[string]json.RawMessage, reserved ...string) (json.RawMessage, error) {
	for key, value := range options {
		if key == "stream" || key == "stream_options" || key == "idempotency_key" || key == "options" || slices.Contains(reserved, key) {
			return nil, newToolError(http.StatusBadRequest, codeInvalidInput, fmt.Sprintf("options.%s is reserved; use the common field where supported. Streaming is unavailable", key), nil)
		}
		common[key] = value
	}
	return json.Marshal(common)
}
