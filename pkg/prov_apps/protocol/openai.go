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

// OpenAIProvider implements FullProvider for any OpenAI-compatible API
// (vLLM, llama.cpp, MLX, etc.).
type OpenAIProvider struct {
	providerName string
	providerType string
	defaultPort  int
	httpClient   *http.Client
	connector    *connectivity.ProviderConnector
	// connected caches a successful first probe so the retry/breaker loop
	// does not run on every call. It is per provider, not per Target: a
	// later endpoint or credential edit is not probed again.
	connected atomic.Bool
	protocol  string
}

// NewOpenAIProvider creates a new OpenAI-compatible provider.
func NewOpenAIProvider(name string, defaultPort int, protocol string, httpClient *http.Client) *OpenAIProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: constants.HTTPDefaultTimeout}
	}
	return &OpenAIProvider{
		providerName: name,
		providerType: utils.NormalizeAppType(name),
		defaultPort:  defaultPort,
		httpClient:   httpClient,
		connector:    connectivity.NewProviderConnector(),
		protocol:     protocol,
	}
}

func (p *OpenAIProvider) Name() string     { return p.providerName }
func (p *OpenAIProvider) Type() string     { return p.providerType }
func (p *OpenAIProvider) DefaultPort() int { return p.defaultPort }

func (p *OpenAIProvider) ensureConnection(ctx context.Context, t Target) error {
	if t.base == "" {
		return errNoEndpoint
	}
	if !t.probe || p.connected.Load() {
		return nil
	}
	if err := p.connector.ConnectToProvider(ctx, p.providerType, t.base, t.up.Header()); err != nil {
		return fmt.Errorf("failed to connect to %s: %w", p.providerType, err)
	}
	p.connected.Store(true)
	return nil
}

func (p *OpenAIProvider) ListModels(ctx context.Context, t Target) ([]ModelInfo, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, constants.HTTPShortTimeout)
		defer cancel()
	}
	if err := p.ensureConnection(ctx, t); err != nil {
		return nil, err
	}

	httpResp, err := t.call(ctx, p.httpClient, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if err := utils.CheckHTTPStatus(httpResp.StatusCode, httpResp.Body, fmt.Sprintf("query %s models", p.providerType)); err != nil {
		return nil, err
	}

	var response struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := utils.DecodeJSONResponse(httpResp.Body, &response); err != nil {
		return nil, err
	}

	models := make([]ModelInfo, 0, len(response.Data))
	for _, m := range response.Data {
		t := time.Unix(m.Created, 0)
		models = append(models, ModelInfo{
			Name:       m.ID,
			FullID:     m.ID,
			Modified:   t.Format("2006-01-02 15:04"),
			ModifiedAt: t,
		})
	}
	return models, nil
}

func (p *OpenAIProvider) ListRunningModels(_ context.Context, _ Target) ([]RunningModelInfo, error) {
	return []RunningModelInfo{}, nil
}

func (p *OpenAIProvider) LoadModel(ctx context.Context, t Target, model string, _ map[string]string) error {
	models, err := p.ListModels(ctx, t)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.Name == model || m.FullID == model {
			return nil
		}
	}
	return fmt.Errorf("model %q not found", model)
}

func (p *OpenAIProvider) UnloadModel(_ context.Context, _ Target, _ string) error {
	return fmt.Errorf("%s: model unloading not implemented", p.providerType)
}

func (p *OpenAIProvider) DeleteModel(_ context.Context, _ Target, _ string) error {
	return fmt.Errorf("%s: model deletion not supported via API", p.providerType)
}

func (p *OpenAIProvider) ShowApp(ctx context.Context, t Target) (map[string]any, error) {
	models, err := p.ListModels(ctx, t)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name": p.Name(), "type": p.Type(),
		"address": t.Host(), "available_models": len(models),
		"status": "configured",
	}, nil
}

func (p *OpenAIProvider) ShowModel(ctx context.Context, t Target, model string) (map[string]any, error) {
	models, err := p.ListModels(ctx, t)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.Name == model || m.FullID == model {
			format := "unknown"
			if strings.Contains(strings.ToLower(model), ".gguf") {
				format = "gguf"
			} else if strings.Contains(model, "/") {
				format = "huggingface"
			}
			return map[string]any{
				"name": m.Name, "full_id": m.FullID, "size": m.Size,
				"modified": m.Modified, "provider": p.Type(), "format": format,
			}, nil
		}
	}
	return nil, fmt.Errorf("model %q not found", model)
}

func (p *OpenAIProvider) DisplayTable(models []DisplayModel, _ string) {
	fmt.Printf("%-50s %-12s %-10s %-12s\n", "NAME", "STATUS", "SIZE", "MODIFIED")
	for _, m := range models {
		status := m.Status
		if status == "" {
			status = "available"
		}
		fmt.Printf("%-50s %-12s %-10s %-12s\n",
			m.Name, status, formatting.FormatSize(m.Size),
			formatting.FormatModifiedTime(m.Modified))
	}
}
