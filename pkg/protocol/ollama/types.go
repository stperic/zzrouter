// Request/response types for the Ollama compatibility surface (/api/*).
// JSON tags MUST marshal to the same bytes the real Ollama daemon produces —
// the Ollama protocol dictates the wire format.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ChatMessage is one entry in a chat request's messages array.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// InferenceRequest is the common subset of /api/generate and /api/chat
// bodies that zzRouter reads for routing. The raw body is still forwarded
// verbatim to the upstream provider.
type InferenceRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Prompt   string        `json:"prompt"` // /api/generate only
	Stream   *bool         `json:"stream"`
}

// PullRequest matches upstream Ollama's PullRequest wire shape. The
// legacy `name` field (alias for `model`, retired in upstream's API
// docs in 2024) is no longer accepted — clients must send `model`.
type PullRequest struct {
	Model    string `json:"model,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	Stream   *bool  `json:"stream,omitempty"`
}

// ManagementRequest covers delete/show/push/copy bodies. Upstream
// Ollama's DeleteRequest and ShowRequest take `model` and still honor
// `name` as an alias for it; CopyRequest uses `source`/`destination`.
// Clients in the wild send either spelling, so both are bound here.
type ManagementRequest struct {
	Model       string `json:"model,omitempty"`
	Name        string `json:"name,omitempty"`
	Verbose     bool   `json:"verbose,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
}

// ModelName returns the request's model identifier — `model` (or its
// `name` alias) for delete/show/push, `source` for copy. Checked in
// that order so a copy body carrying both keeps `source` as the
// subject and a show body carrying both prefers upstream's primary.
func (r *ManagementRequest) ModelName() string {
	if r.Model != "" {
		return r.Model
	}
	if r.Name != "" {
		return r.Name
	}
	return r.Source
}

// ModelCapabilities reads explicit capabilities from the model's daemon.
func ModelCapabilities(ctx context.Context, client *http.Client, daemon *backend.Resolved, model string) ([]string, bool) {
	body, _ := json.Marshal(ManagementRequest{Model: model})
	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var result struct {
		Capabilities []any `json:"capabilities"`
	}
	raw, err := utils.ReadCapped(resp.Body, constants.MaxClusterBodySize)
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Capabilities == nil {
		return nil, false
	}
	capabilities := make([]string, 0, len(result.Capabilities))
	for _, value := range result.Capabilities {
		capability, ok := value.(string)
		if !ok {
			return nil, false
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities, true
}

// ModelDetails carries the per-model metadata block Ollama returns
// inside tag and ps entries.
type ModelDetails struct {
	Format        string `json:"format"`
	Family        string `json:"family"`
	ParameterSize string `json:"parameter_size"`
	QuantLevel    string `json:"quantization_level"`
}

// TagEntry is one model in the /api/tags response.
type TagEntry struct {
	Name        string       `json:"name"`
	Model       string       `json:"model"`
	RemoteModel string       `json:"remote_model,omitempty"`
	RemoteHost  string       `json:"remote_host,omitempty"`
	Size        int64        `json:"size"`
	ModifiedAt  time.Time    `json:"modified_at"`
	Digest      string       `json:"digest"`
	Details     ModelDetails `json:"details"`
}

// TagsResponse is the /api/tags envelope.
type TagsResponse struct {
	Models []TagEntry `json:"models"`
}

// PsEntry is one running model in the /api/ps response.
type PsEntry struct {
	// Provider is internal admission metadata, stripped before compatibility responses.
	Provider      string       `json:"provider,omitempty"`
	Name          string       `json:"name"`
	Model         string       `json:"model"`
	Size          int64        `json:"size"`
	Digest        string       `json:"digest"`
	SizeVRAM      int64        `json:"size_vram"`
	ContextLength int          `json:"context_length"`
	ExpiresAt     time.Time    `json:"expires_at"`
	Details       ModelDetails `json:"details"`
}

// PsResponse is the /api/ps envelope.
type PsResponse struct {
	Models []PsEntry `json:"models"`
}
