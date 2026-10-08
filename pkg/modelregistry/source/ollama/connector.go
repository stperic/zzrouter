// Package ollama is the Ollama-specific source connector under
// pkg/modelregistry/source. It talks to the user's LOCAL Ollama daemon
// (configurable endpoint, typically http://localhost:11434) — list
// tags, pull a model with streaming progress, delete a model.
//
// This is deliberately distinct from pkg/modelregistry/search's Ollama
// code, which scrapes the PUBLIC web catalog at ollama.com/library for
// discovery. Different base URLs, different timeouts (30s here vs. 5s
// fail-fast on the catalog side), different systems entirely. Do not
// unify — there is no useful abstraction between "my daemon on
// localhost" and "the upstream marketplace website."
//
// Invariant: this package must not import pkg/modelregistry. The
// orchestrator depends on us, not the other way around — see
// pkg/modelregistry/source/internal_import_guard_test.go.
package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ErrNotConfigured is returned by Connector methods when Ollama is
// disabled or has no endpoint in the current AppsConfig. Callers that
// want to treat "not configured" as a non-error (e.g. catalog lookups)
// should compare with errors.Is.
//
// Contract for the "silent success" methods on Connector:
//   - ListModels returns (nil, nil) when not configured — aggregating callers
//     (Registry.buildModelList) can append without branching.
//   - ModelExists returns (false, nil) when not configured — source-specific
//     existence checks fold into a generic "model not here" answer.
//   - DeleteModel, PullModel*, Endpoint return ErrNotConfigured — those
//     operations have no meaningful silent answer.
//
// Callers that need to distinguish "not configured" from "configured but
// returned no models" should call IsConfigured before the operation.
var ErrNotConfigured = errors.New("ollama not configured")

// state bundles every field that can change at runtime on a configured
// connector. Using a single pointer swap (vs. per-field atomics) guarantees
// readers never see a torn state where e.g. the endpoint is updated but the
// credential is stale: the daemon's endpoint and what it is sent are one
// resolved value.
type state struct {
	daemon backend.Resolved
}

// Connector handles all Ollama-specific operations. It is always
// constructed in a "not configured" state; call ReloadConfig with the
// current AppsConfig to activate it. ReloadConfig is safe to call at any
// time (startup seed or runtime config change) and is the single hook the
// server uses — no "if ollama" branches should leak into caller code.
//
// Thread safety: state is held in an atomic pointer, so reads (Endpoint,
// IsConfigured, each HTTP method) are lock-free and never block concurrent
// writes. A request that loaded the previous state runs to completion
// against it even if ReloadConfig swaps the pointer mid-flight.
type Connector struct {
	client *http.Client
	state  atomic.Pointer[state] // nil = not configured
}

// LayerProgress tracks progress for individual layers during a pull.
type LayerProgress struct {
	Digest    string
	Total     int64
	Completed int64
}

// APITimeout is the default timeout for non-streaming Ollama API calls
// (list, show, delete). PullModel uses per-request contexts with no
// timeout since it streams.
const APITimeout = 30 * time.Second

// NewConnector creates a new Ollama connector in the "not configured"
// state. Call ReloadConfig with the current AppsConfig to activate it.
func NewConnector() *Connector {
	return &Connector{
		client: &http.Client{Timeout: APITimeout},
	}
}

// ReloadConfig reconfigures the connector from the current AppsConfig.
// When Ollama is enabled with a non-empty runtime endpoint, the connector
// becomes active; otherwise it returns to the "not configured" state.
// This method is the authoritative place for Ollama-specific config reading
// — higher-level code (Registry, server listener) stays provider-agnostic.
//
// Safe to call concurrently with in-flight requests: the state swap is a
// single atomic store, and any request that already loaded the previous
// state runs to completion against it.
func (oc *Connector) ReloadConfig(cfg *config.AppsConfig) {
	daemon, ok := backend.NewResolver(func() *config.AppsConfig { return cfg }).Resolve(constants.AppOllama)
	if !ok {
		oc.state.Store(nil)
		return
	}
	oc.state.Store(&state{daemon: *daemon})
}

// IsConfigured reports whether ReloadConfig has been called with a config
// that enables Ollama. Use this as a fast-path when the caller would
// otherwise have to call an HTTP method and inspect for ErrNotConfigured —
// for example, Registry.PullOllamaModelAsync checks before spawning a
// goroutine to avoid starting work that is guaranteed to fail.
func (oc *Connector) IsConfigured() bool {
	return oc.state.Load() != nil
}

// Daemon returns the configured Ollama: its endpoint and what it is
// sent, or ErrNotConfigured if the connector is inactive. This is the
// single source of truth for "where is Ollama and how is it reached";
// other subsystems (HTTP proxy, inference passthrough) must go through
// here instead of re-reading provider config themselves.
func (oc *Connector) Daemon() (*backend.Resolved, error) {
	st := oc.state.Load()
	if st == nil {
		return nil, ErrNotConfigured
	}
	daemon := st.daemon
	return &daemon, nil
}

// ModelExists checks if a model exists in the Ollama library.
// This checks the Ollama public library, not locally installed models.
// Returns (false, nil) when Ollama is not configured — the model simply
// cannot be resolved through this source.
//
// ctx carries the caller's deadline and cancellation; a cancelled
// context aborts the in-flight HTTP request immediately.
func (oc *Connector) ModelExists(ctx context.Context, modelName string) (bool, error) {
	daemon, err := oc.Daemon()
	if err != nil {
		return false, nil
	}
	// Try to get model info using /api/show endpoint
	// This will return 404 if model doesn't exist in library

	reqBody := map[string]string{
		"name": modelName,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}

	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", strings.NewReader(string(bodyBytes)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := oc.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	// 200 = model exists, 404 = model not found
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}

	return false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// ListModels queries Ollama API for available models on the connector's
// own endpoint (the managed Ollama install). Returns a nil slice and nil
// error when the connector is not configured so callers can accumulate
// results from multiple sources without branching.
//
// Registry.buildModelList no longer goes through here when iterating
// multiple ollama-protocol providers — it calls ListTags directly so
// each backend can be tagged with its own provider key.
//
// ctx carries the caller's deadline and cancellation.
func (oc *Connector) ListModels(ctx context.Context) ([]*metadata.ModelMetadata, error) {
	daemon, err := oc.Daemon()
	if err != nil {
		return nil, nil //nolint:nilerr // unconfigured Ollama is a soft outcome (empty list), not a caller error
	}
	return oc.ListTags(ctx, daemon, constants.AppOllama)
}

// ListTags performs GET /api/tags on daemon using the connector's HTTP
// client and returns the parsed model list, tagging each entry with
// AssignedApp=providerName so the routing pipeline can dispatch
// per-backend when multiple protocol:ollama external providers are
// configured.
//
// Callers pass a daemon other than the connector's own when they need to
// enumerate external Ollama backends (ollama-connect instances); the
// connector's HTTP timeouts still apply.
func (oc *Connector) ListTags(ctx context.Context, daemon *backend.Resolved, providerName string) ([]*metadata.ModelMetadata, error) {
	req, err := daemon.NewRequest(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, err
	}

	resp, err := oc.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama API returned status %d", resp.StatusCode)
	}

	// Parse full Ollama response with all details
	var result struct {
		Models []struct {
			Name       string    `json:"name"`
			Model      string    `json:"model"`
			ModifiedAt time.Time `json:"modified_at"`
			Size       int64     `json:"size"`
			Digest     string    `json:"digest"`
			Details    struct {
				ParentModel       string   `json:"parent_model"`
				Format            string   `json:"format"`
				Family            string   `json:"family"`
				Families          []string `json:"families"`
				ParameterSize     string   `json:"parameter_size"`
				QuantizationLevel string   `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var models []*metadata.ModelMetadata
	// Bound the whole evidence sweep so many unavailable models cannot delay a scan indefinitely.
	evidenceCtx, cancel := context.WithTimeout(ctx, constants.HTTPShortTimeout)
	defer cancel()
	for _, m := range result.Models {
		// Store full Ollama details in Extra field
		extra := map[string]any{
			"model":       m.Model,
			"modified_at": m.ModifiedAt,
			"details": map[string]any{
				"parent_model":       m.Details.ParentModel,
				"format":             m.Details.Format,
				"family":             m.Details.Family,
				"families":           m.Details.Families,
				"parameter_size":     m.Details.ParameterSize,
				"quantization_level": m.Details.QuantizationLevel,
			},
		}

		if vision, known := oc.modelVision(evidenceCtx, daemon, m.Name); known {
			details, _ := extra["details"].(map[string]any)
			details["vision"] = vision
		}
		models = append(models, &metadata.ModelMetadata{
			Name:             m.Name,
			FullPath:         "", // Ollama manages its own files
			Format:           m.Details.Format,
			Quantization:     m.Details.QuantizationLevel,
			Size:             m.Size,
			SourceRepo:       metadata.SourceOllama,
			SourceID:         m.Name,
			Modified:         m.ModifiedAt,
			Digest:           m.Digest,
			AddedAt:          m.ModifiedAt,
			Extra:            extra,
			AssignedApp:      providerName,
			AssignedNode:     "localhost",
			AssignmentSource: "auto",
			AssignedAt:       utils.Now(),
		})
	}

	return models, nil
}

// DeleteModel deletes a model from Ollama.
//
// ctx carries the caller's deadline and cancellation.
func (oc *Connector) DeleteModel(ctx context.Context, modelName string) error {
	daemon, err := oc.Daemon()
	if err != nil {
		return err
	}

	body := map[string]string{
		"name": modelName,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	// Ollama delete uses DELETE method with JSON body
	req, err := daemon.NewRequest(ctx, http.MethodDelete, "/api/delete", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := oc.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama delete failed with status %d", resp.StatusCode)
	}

	return nil
}

// PullModelWithContext initiates a model pull in Ollama using the provided context.
// Cancelling the context aborts the HTTP streaming request immediately.
func (oc *Connector) PullModelWithContext(ctx context.Context, modelName string, progressCallback func(status, digest string, total, completed int64)) error {
	daemon, err := oc.Daemon()
	if err != nil {
		return err
	}

	body := map[string]string{
		"name": modelName,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/pull", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a dedicated client with no overall timeout but with a response header timeout
	// so the initial connection can fail fast while allowing long streaming deploy responses
	streamClient := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama deploy failed with status %d", resp.StatusCode)
	}

	// Track layers to calculate total progress
	layers := make(map[string]LayerProgress)

	// Stream progress updates
	decoder := json.NewDecoder(resp.Body)
	for {
		var progress struct {
			Status    string  `json:"status"`
			Digest    string  `json:"digest,omitempty"`
			Total     float64 `json:"total,omitempty"`
			Completed float64 `json:"completed,omitempty"`
		}

		if err := decoder.Decode(&progress); err != nil {
			// Stream ended cleanly.
			if errors.Is(err, io.EOF) {
				break
			}
			// Decoder returns *json.SyntaxError on a truncated/empty frame
			// between NDJSON records — keep reading.
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				continue
			}
			return fmt.Errorf("failed to decode progress: %w", err)
		}

		// Track layer progress if we have digest and size info
		if progress.Digest != "" && progress.Total > 0 {
			layer := layers[progress.Digest]
			layer.Digest = progress.Digest
			layer.Total = int64(progress.Total)
			layer.Completed = int64(progress.Completed)
			layers[progress.Digest] = layer
		}

		// Calculate aggregate progress across all layers
		var totalBytes, completedBytes int64
		for _, l := range layers {
			totalBytes += l.Total
			completedBytes += l.Completed
		}

		// Call progress callback with aggregated values
		if progressCallback != nil {
			progressCallback(progress.Status, progress.Digest, totalBytes, completedBytes)
		}

		// Check if deploy is complete
		if progress.Status == "success" {
			break
		}
	}

	return nil
}

// modelVision reads engine evidence without making a failed probe remove a model.
func (oc *Connector) modelVision(ctx context.Context, daemon *backend.Resolved, name string) (bool, bool) {
	body, err := json.Marshal(map[string]string{"model": name})
	if err != nil {
		return false, false
	}
	req, err := daemon.NewRequest(ctx, http.MethodPost, "/api/show", strings.NewReader(string(body)))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := oc.client.Do(req)
	if err != nil {
		return false, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var result struct {
		Capabilities *[]string `json:"capabilities"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.Capabilities == nil {
		return false, false
	}
	return slices.Contains(*result.Capabilities, "vision"), true
}
