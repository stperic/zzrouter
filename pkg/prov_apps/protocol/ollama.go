package protocol

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stperic/zzrouter/pkg/connectivity"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/prov_apps/protocol/formatting"
	"github.com/stperic/zzrouter/pkg/utils"
)

// OllamaProvider implements FullProvider for the Ollama API.
type OllamaProvider struct {
	name       string
	httpClient *http.Client
	connector  *connectivity.ProviderConnector
	// connected caches a successful first probe so the retry/breaker loop
	// does not run on every call. It is per provider, not per Target: a
	// later endpoint or credential edit is not probed again.
	connected atomic.Bool
}

// NewOllamaProvider creates a new Ollama provider.
func NewOllamaProvider(httpClient *http.Client) *OllamaProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: constants.HTTPDefaultTimeout}
	}
	return &OllamaProvider{
		name:       "ollama",
		httpClient: httpClient,
		connector:  connectivity.NewProviderConnector(),
	}
}

func (p *OllamaProvider) Name() string     { return p.name }
func (p *OllamaProvider) Type() string     { return "ollama" }
func (p *OllamaProvider) DefaultPort() int { return constants.DefaultOllamaPort }
func (p *OllamaProvider) ensureConnection(ctx context.Context, t Target) error {
	if t.base == "" {
		return errNoEndpoint
	}
	if !t.probe || p.connected.Load() {
		return nil
	}
	if err := p.connector.ConnectToProvider(ctx, "ollama", t.base, t.up.Header()); err != nil {
		return fmt.Errorf("failed to connect to Ollama: %w", err)
	}
	p.connected.Store(true)
	return nil
}

func (p *OllamaProvider) ListModels(ctx context.Context, t Target) ([]ModelInfo, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, constants.HTTPShortTimeout)
		defer cancel()
	}
	if err := p.ensureConnection(ctx, t); err != nil {
		return nil, err
	}

	httpResp, err := t.call(ctx, p.httpClient, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if err := utils.CheckHTTPStatus(httpResp.StatusCode, httpResp.Body, "query Ollama models"); err != nil {
		return nil, err
	}

	var resp struct {
		Models []struct {
			Name       string    `json:"name"`
			Model      string    `json:"model"`
			Size       int64     `json:"size"`
			ModifiedAt time.Time `json:"modified_at"`
			Digest     string    `json:"digest"`
			Details    struct {
				Format        string `json:"format"`
				Family        string `json:"family"`
				ParameterSize string `json:"parameter_size"`
				QuantLevel    string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := utils.DecodeJSONResponse(httpResp.Body, &resp); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(resp.Models))
	for _, m := range resp.Models {
		models = append(models, ModelInfo{
			Name:       m.Name,
			FullID:     m.Name,
			Size:       m.Size,
			Modified:   m.ModifiedAt.Format("2006-01-02 15:04"),
			ModifiedAt: m.ModifiedAt,
			Digest:     m.Digest,
			Extra: map[string]any{
				"format":         m.Details.Format,
				"family":         m.Details.Family,
				"parameter_size": m.Details.ParameterSize,
				"quant_level":    m.Details.QuantLevel,
			},
		})
	}
	return models, nil
}

func (p *OllamaProvider) ListRunningModels(ctx context.Context, t Target) ([]RunningModelInfo, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, constants.HTTPShortTimeout)
		defer cancel()
	}
	if err := p.ensureConnection(ctx, t); err != nil {
		return nil, err
	}

	httpResp, err := t.call(ctx, p.httpClient, http.MethodGet, "/api/ps", nil)
	if err != nil {
		return nil, err
	}
	if err := utils.CheckHTTPStatus(httpResp.StatusCode, httpResp.Body, "query running models"); err != nil {
		return nil, err
	}

	var resp struct {
		Models []struct {
			Name          string `json:"name"`
			Size          int64  `json:"size"`
			Digest        string `json:"digest"`
			SizeVRAM      int64  `json:"size_vram"`
			ContextLength int    `json:"context_length"`
			ExpiresAt     string `json:"expires_at"`
			Details       struct {
				Format        string `json:"format"`
				Family        string `json:"family"`
				ParameterSize string `json:"parameter_size"`
				QuantLevel    string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := utils.DecodeJSONResponse(httpResp.Body, &resp); err != nil {
		return nil, err
	}

	result := make([]RunningModelInfo, 0, len(resp.Models))
	for _, m := range resp.Models {
		expiresAt, _ := time.Parse(time.RFC3339, m.ExpiresAt)
		result = append(result, RunningModelInfo{
			Name:          m.Name,
			Digest:        m.Digest,
			ExpiresAt:     expiresAt,
			ContextLength: m.ContextLength,
			Size:          m.Size,
			SizeVRAM:      m.SizeVRAM,
			Format:        m.Details.Format,
			Family:        m.Details.Family,
			ParameterSize: m.Details.ParameterSize,
			QuantLevel:    m.Details.QuantLevel,
		})
	}
	return result, nil
}

func (p *OllamaProvider) LoadModel(ctx context.Context, t Target, model string, params map[string]string) error {
	req := map[string]any{"model": model, "prompt": "", "stream": false}
	for key, value := range params {
		req[strings.ReplaceAll(key, "-", "_")] = value
	}
	resp, err := t.call(ctx, p.httpClient, http.MethodPost, "/api/generate", req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama load failed (status %d): %s", resp.StatusCode, string(resp.Body))
	}
	return nil
}

func (p *OllamaProvider) UnloadModel(ctx context.Context, t Target, model string) error {
	resp, err := t.call(ctx, p.httpClient, http.MethodPost, "/api/generate", map[string]any{"model": model, "keep_alive": 0})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama unload failed (status %d): %s", resp.StatusCode, string(resp.Body))
	}
	return nil
}

func (p *OllamaProvider) DeleteModel(ctx context.Context, t Target, model string) error {
	resp, err := t.call(ctx, p.httpClient, http.MethodDelete, "/api/delete", map[string]string{"name": model})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama delete failed (status %d): %s", resp.StatusCode, string(resp.Body))
	}
	return nil
}

func (p *OllamaProvider) ShowApp(ctx context.Context, t Target) (map[string]any, error) {
	var version string
	if httpResp, err := t.call(ctx, p.httpClient, http.MethodGet, "/api/version", nil); err == nil && httpResp.StatusCode == http.StatusOK {
		var v struct {
			Version string `json:"version"`
		}
		if utils.DecodeJSONResponse(httpResp.Body, &v) == nil {
			version = v.Version
		}
	}

	running, _ := p.ListRunningModels(ctx, t)
	available, _ := p.ListModels(ctx, t)

	return map[string]any{
		"name": p.Name(), "type": p.Type(), "version": version,
		"address": t.Host(), "running_models": len(running),
		"available_models": len(available), "status": "connected",
	}, nil
}

func (p *OllamaProvider) ShowModel(ctx context.Context, t Target, model string) (map[string]any, error) {
	models, err := p.ListModels(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.Name == model {
			result := map[string]any{
				"name": m.Name, "size": m.Size, "modified": m.Modified,
				"digest": m.Digest, "provider": p.Type(),
			}
			running, _ := p.ListRunningModels(ctx, t)
			for _, r := range running {
				if r.Name == model {
					result["running"] = true
					result["context_length"] = r.ContextLength
					result["size_vram"] = r.SizeVRAM
					break
				}
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("model %s not found", model)
}

func (p *OllamaProvider) DisplayTable(models []DisplayModel, command string) {
	if command == "ps" {
		fmt.Printf("%-35s %-15s %-10s %-10s %-8s %-12s\n", "NAME", "ID", "SIZE", "PROCESSOR", "CONTEXT", "UNTIL")
		for _, m := range models {
			if strings.ToLower(m.Status) != "loaded" {
				continue
			}
			fmt.Printf("%-35s %-15s %-10s %-10s %-8d %-12s\n",
				m.Name, truncate(m.Digest, 15), formatting.FormatSize(m.SizeVRAM),
				"100% GPU", m.ContextLength, m.ExpiresAt)
		}
	} else {
		fmt.Printf("%-36s %-15s %-10s %-12s\n", "NAME", "ID", "SIZE", "MODIFIED")
		for _, m := range models {
			fmt.Printf("%-36s %-15s %-10s %-12s\n",
				m.Name, truncate(m.Digest, 15), formatting.FormatSize(m.Size),
				formatting.FormatModifiedTime(m.Modified))
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}
