package connectivity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/fallback"
	"github.com/stperic/zzrouter/pkg/retry"
	"github.com/stperic/zzrouter/pkg/utils"
)

// ProviderConnector handles robust connections to AI apps (Ollama, vLLM, etc.)
type ProviderConnector struct {
	backoff         retry.Backoff
	maxRetries      int
	httpClient      *http.Client
	circuitBreakers *fallback.BreakerManager
}

// NewProviderConnector creates a new provider connector with retry logic and circuit breakers
func NewProviderConnector() *ProviderConnector {
	return &ProviderConnector{
		backoff: retry.Backoff{
			// First retry at 200ms; subsequent retries grow by 2.5×,
			// capped at 15s. Provider-optimized: tighter than the
			// cluster-mesh policy because provider flap windows are
			// shorter than peer-reachability windows.
			Initial:    200 * time.Millisecond,
			Max:        15 * time.Second,
			Multiplier: 2.5,
			Jitter:     0.2, // ±20%
		},
		maxRetries: 4,
		httpClient: &http.Client{
			Timeout: constants.HealthCheckTimeout, // Reasonable timeout for provider APIs
		},
		circuitBreakers: fallback.NewBreakerManager(),
	}
}

// ConnectToProvider establishes a robust connection to an AI provider with
// circuit-breaker + backoff protection. A nil return means the provider
// answered at least once; callers can treat that as "connected" and proceed
// to first real API call. No handshake payload is exchanged — version and
// model listing happen at the provider layer on demand. header
// authenticates the probe as the provider's own calls are.
func (pc *ProviderConnector) ConnectToProvider(ctx context.Context, providerType, providerURL string, header http.Header) error {
	breaker := pc.circuitBreakers.GetBreaker(providerURL)
	if breaker.IsOpen() {
		return fmt.Errorf("circuit breaker open for %s provider (will retry automatically)", providerType)
	}

	return breaker.Execute(func() error {
		var lastErr error
		for attempt := 0; attempt < pc.maxRetries; attempt++ {
			if attempt > 0 {
				delay := pc.backoff.Delay(attempt - 1)
				utils.LogInfof("Retrying %s connection in %v (attempt %d/%d)",
					providerType, delay, attempt+1, pc.maxRetries)
				if err := retry.Sleep(ctx, delay); err != nil {
					return fmt.Errorf("connection cancelled: %w", err)
				}
			}

			err := pc.probe(ctx, providerURL, header)
			if err == nil {
				return nil
			}
			lastErr = err

			if pc.isPermanentProviderError(err) {
				utils.LogWarnf("Permanent error with %s provider: %v", providerType, err)
				return err
			}
		}
		return fmt.Errorf("failed after %d attempts: %w", pc.maxRetries, lastErr)
	})
}

// probe issues a lightweight GET against the OpenAI-compatible /v1/models
// endpoint. We only care that the provider answers with a non-permanent
// status — the response body is discarded. Ollama's /v1/models is also
// OpenAI-shaped, so one path serves every supported provider.
func (pc *ProviderConnector) probe(ctx context.Context, providerURL string, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, providerURL+"/v1/models", nil)
	if err != nil {
		// A malformed URL cannot succeed on retry.
		return fmt.Errorf("failed to create models request: %w: %w", ErrPermanent, err)
	}
	maps.Copy(req.Header, header)

	resp, err := pc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("models request failed: %w", err)
	}
	defer func() {
		// Drain before close so the underlying TCP conn can be reused by the
		// keep-alive pool across retry attempts.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		if permanentHTTPStatus(resp.StatusCode) {
			return fmt.Errorf("models endpoint returned HTTP %d: %w", resp.StatusCode, ErrPermanent)
		}
		return fmt.Errorf("models endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// isPermanentProviderError reports whether an error is unrecoverable and should
// short-circuit retry. Permanence is signaled by errors wrapped with ErrPermanent
// (typically at the HTTP boundary for 401/403/405 or malformed-request paths) or
// by typed network errors such as DNS NotFound. Transient conditions
// (connection refused, timeout, reset) are intentionally not matched here so
// callers fall through to backoff.
func (pc *ProviderConnector) isPermanentProviderError(err error) bool {
	return errors.Is(err, ErrPermanent) || isPermanentNetError(err)
}
