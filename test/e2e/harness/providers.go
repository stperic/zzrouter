package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ResolvedValue mirrors pkg/config.ResolvedValue (subset). Tier is
// one of "default", "model", "node", "node-model", "endpoint",
// "request".
type ResolvedValue struct {
	Value    string `json:"value"`
	Tier     string `json:"tier"`
	Node     string `json:"node,omitempty"`
	Model    string `json:"model,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// ResolvedParams mirrors pkg/config.ResolvedParams DTO.
type ResolvedParams struct {
	Parameters  map[string]ResolvedValue `json:"parameters"`
	Environment map[string]ResolvedValue `json:"environment"`
}

// GetResolvedParams calls GET /zzrouter/v1/providers/:name/resolved
// with optional query params.
func GetResolvedParams(ctx context.Context, c *Client, provider string, query string) (*ResolvedParams, error) {
	path := "/zzrouter/v1/providers/" + provider + "/resolved"
	if query != "" {
		path += "?" + query
	}
	resp, err := c.GET(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("%s: status %d body=%s", path, resp.Status, resp.Body)
	}
	out := &ResolvedParams{}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return nil, fmt.Errorf("decode resolved: %w", err)
	}
	return out, nil
}

// PatchProviderParameters PATCHes the merge-patch endpoint. Returns
// the raw Response so tests can inspect status + body for error-path
// assertions (a 400 with a closed-enum code is a contract assertion,
// not a transport failure).
func PatchProviderParameters(ctx context.Context, c *Client, provider string, body any) (Response, error) {
	path := "/zzrouter/v1/providers/" + provider + "/parameters"
	encoded, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("encode patch body: %w", err)
	}
	return c.PATCH(ctx, path, nil, RawBody("application/merge-patch+json", encoded))
}

// PatchProviderParametersOK is the convenience wrapper that asserts a
// 200 status and returns the resolved view echoed by the server.
func PatchProviderParametersOK(ctx context.Context, c *Client, provider string, body any) (*ResolvedParams, error) {
	resp, err := PatchProviderParameters(ctx, c, provider, body)
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("PATCH /providers/%s/parameters: status %d body=%s", provider, resp.Status, resp.Body)
	}
	out := &ResolvedParams{}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return nil, fmt.Errorf("decode resolved post-patch: %w", err)
	}
	return out, nil
}

// ParamErrorCode is the closed-enum error code returned by validation
// failures on PATCH /providers/:name/parameters. Mirrors
// pkg/httperr.ParamErrorCode.
type ParamErrorCode string

const (
	ParamErrUnknownFlag    ParamErrorCode = "unknown_flag"
	ParamErrWrongType      ParamErrorCode = "wrong_type"
	ParamErrOutOfRange     ParamErrorCode = "out_of_range"
	ParamErrUnknownNode    ParamErrorCode = "unknown_node"
	ParamErrUnknownModel   ParamErrorCode = "unknown_model"
	ParamErrCoercionFailed ParamErrorCode = "coercion_failed"
)

// ParamErrorBody is the RFC 9457 problem envelope, read for its
// per-key `errors` array. There is no second envelope: a validation
// failure and a not-found both arrive here, which is why this type
// carries the problem fields alongside the array.
type ParamErrorBody struct {
	Type   string       `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Code   string       `json:"code,omitempty"`
	Errors []ParamError `json:"errors"`
}

type ParamError struct {
	Key     string         `json:"key"`
	Code    ParamErrorCode `json:"code"`
	Message string         `json:"message"`
	Want    string         `json:"want,omitempty"`
	Hint    string         `json:"hint,omitempty"`
}

// ModelEntry mirrors the model row in GET /zzrouter/v1/models (subset).
// The list endpoint returns a bare {data:[…]} envelope (no
// success/message wrapper).
type ModelEntry struct {
	Name        string `json:"name"`
	Model       string `json:"model"`
	SourceID    string `json:"source_id,omitempty"`
	SourceRepo  string `json:"source_repo,omitempty"`
	Node        string `json:"node,omitempty"`
	AssignedApp string `json:"assigned_app,omitempty"`
	Format      string `json:"format,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type modelListEnvelope struct {
	Data []ModelEntry `json:"data"`
}

// ListModels fetches GET /zzrouter/v1/models. Optional query (e.g.
// "limit=10&node=worker-1").
func ListModels(ctx context.Context, c *Client, query string) ([]ModelEntry, error) {
	path := "/zzrouter/v1/models"
	if query != "" {
		path += "?" + query
	}
	resp, err := c.GET(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("%s: status %d body=%s", path, resp.Status, resp.Body)
	}
	var env modelListEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode models list: %w", err)
	}
	return env.Data, nil
}

// ModelStats mirrors the data block of GET /zzrouter/v1/models/stats.
type ModelStats struct {
	ByFormat    map[string]int `json:"by_format"`
	BySource    map[string]int `json:"by_source"`
	TotalModels int            `json:"total_models"`
	TotalSize   int64          `json:"total_size"`
}

type modelStatsEnvelope struct {
	Data ModelStats `json:"data"`
}

// GetModelStats fetches GET /zzrouter/v1/models/stats.
func GetModelStats(ctx context.Context, c *Client) (*ModelStats, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/models/stats")
	if err != nil {
		return nil, fmt.Errorf("GET /models/stats: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("/models/stats: status %d body=%s", resp.Status, resp.Body)
	}
	var env modelStatsEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode model stats: %w", err)
	}
	return &env.Data, nil
}

// RescanModelsResult mirrors the data block of POST /models/scan.
type RescanModelsResult struct {
	Message     string `json:"message"`
	ModelsFound int    `json:"models_found"`
}

type rescanEnvelope struct {
	Data RescanModelsResult `json:"data"`
}

// RescanModels POSTs /zzrouter/v1/models/scan.
func RescanModels(ctx context.Context, c *Client) (*RescanModelsResult, error) {
	resp, err := c.POST(ctx, "/zzrouter/v1/models/scan", nil)
	if err != nil {
		return nil, fmt.Errorf("POST /models/scan: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("/models/scan: status %d body=%s", resp.Status, resp.Body)
	}
	var env rescanEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode rescan: %w", err)
	}
	return &env.Data, nil
}

// ModelCard is the data block of GET /models/card/:provider/*id. Only
// fields the test asserts are typed; the upstream registries (HF,
// Ollama) return wide payloads we don't need to model fully.
type ModelCard struct {
	Author      string `json:"author,omitempty"`
	Description string `json:"description,omitempty"`
	License     string `json:"license,omitempty"`
	Downloads   int64  `json:"downloads,omitempty"`
}

type modelCardEnvelope struct {
	Data ModelCard `json:"data"`
}

// GetModelCard fetches GET /models/card/:provider/*id.
func GetModelCard(ctx context.Context, c *Client, provider, id string) (*ModelCard, error) {
	path := "/zzrouter/v1/models/card/" + provider + "/" + id
	resp, err := c.GET(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("%s: status %d body=%s", path, resp.Status, resp.Body)
	}
	var env modelCardEnvelope
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode model card: %w", err)
	}
	return &env.Data, nil
}

// DecodeParamError parses a problem+json body and its per-key errors.
func DecodeParamError(body []byte) (*ParamErrorBody, error) {
	out := &ParamErrorBody{}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decode param error: %w", err)
	}
	return out, nil
}
