// Package openai reads models advertised by external HTTP OpenAI-compatible providers.
// An external transport may still have a zzRouter-managed installation.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

const (
	listTimeout = 10 * time.Second
	maxListBody = 4 << 20
)

// ListModels returns only models advertised by the selected external backend.
// Its request carries the backend's credential, never the caller's credential.
func ListModels(ctx context.Context, daemon *backend.Resolved, provider string) ([]*metadata.ModelMetadata, error) {
	selected := *daemon
	selected.Endpoint = config.NormalizeEndpoint(selected.Endpoint)
	req, err := selected.NewRequest(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, fmt.Errorf("create model-list request: %w", err)
	}
	client := &http.Client{Timeout: listTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request model list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model list returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil {
		return nil, fmt.Errorf("read model list: %w", err)
	}
	if len(data) > maxListBody {
		return nil, fmt.Errorf("model list exceeds %d bytes", maxListBody)
	}
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Data == nil {
		return nil, fmt.Errorf("model list must contain a data array")
	}
	models := make([]*metadata.ModelMetadata, 0, len(envelope.Data))
	for _, raw := range envelope.Data {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			return nil, fmt.Errorf("model list contains an invalid model id")
		}
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("model list contains an invalid model id")
		}
		model := &metadata.ModelMetadata{
			Name: id, SourceID: id, SourceRepo: metadata.SourceExternalOpenAI,
			AssignedApp: provider, Backend: selected.Endpoint,
		}
		details := map[string]any{}
		var status string
		if json.Unmarshal(entry["status"], &status) == nil && status != "" {
			details["upstream_status"] = status
		}
		var loaded bool
		if json.Unmarshal(entry["loaded"], &loaded) == nil {
			details["upstream_loaded"] = loaded
		}
		if len(details) > 0 {
			model.Extra = map[string]any{"details": details}
		}
		models = append(models, model)
	}
	return models, nil
}
