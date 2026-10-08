package search

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stperic/zzrouter/pkg/retry"
)

// HTTP timeouts and retry knobs.
//
// httpTimeout is aggressive (5s) so the search path fails fast when a
// source is unreachable — the caller is typically an interactive request
// that would rather degrade to fewer results than stall. Shared between
// the HF client, Ollama scraper, and model-card fetchers.
const (
	httpTimeout       = 5 * time.Second
	defaultMaxRetries = 3
	// rateLimitMaxDelay caps the 429-branch backoff. 429s deserve a longer
	// pause than generic 5xx; this ceiling keeps us responsive for the
	// interactive search caller while still backing off hard.
	rateLimitMaxDelay = 60 * time.Second
)

type retryConfig struct {
	backoff        retry.Backoff
	rateLimitMax   time.Duration
	maxRetries     int
	retryableCodes map[int]bool
}

func defaultRetryConfig() retryConfig {
	return retryConfig{
		backoff: retry.Backoff{
			Initial:    1 * time.Second,
			Max:        30 * time.Second,
			Multiplier: 2.0,
			Jitter:     0.25, // ±25% to avoid thundering-herd retries
		},
		rateLimitMax: rateLimitMaxDelay,
		maxRetries:   defaultMaxRetries,
		retryableCodes: map[int]bool{
			429: true, // Too Many Requests (rate limited)
			500: true, // Internal Server Error
			502: true, // Bad Gateway
			503: true, // Service Unavailable
			504: true, // Gateway Timeout
		},
	}
}

// doRequestWithRetry performs an HTTP request with retry logic and
// exponential backoff. The request is cloned per attempt so bodies that
// rewind on the wire (strings.Reader, bytes.Buffer, nil) work correctly.
func doRequestWithRetry(ctx context.Context, client *http.Client, req *http.Request, config retryConfig) (*http.Response, error) {
	var lastErr error
	var lastStatus int

	for attempt := 0; attempt <= config.maxRetries; attempt++ {
		if attempt > 0 {
			delay := config.backoff.Delay(attempt - 1)
			// Rate-limited responses pause twice as long, capped separately
			// so 429 backs off harder than generic 5xx without bloating the
			// base policy.
			if lastStatus == http.StatusTooManyRequests {
				if doubled := delay * 2; doubled < config.rateLimitMax {
					delay = doubled
				} else {
					delay = config.rateLimitMax
				}
			}
			if err := retry.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}

		reqCopy := req.Clone(ctx)
		resp, err := client.Do(reqCopy)
		if err != nil {
			lastErr = err
			lastStatus = 0
			if attempt < config.maxRetries {
				continue
			}
			break
		}

		if config.retryableCodes[resp.StatusCode] && attempt < config.maxRetries {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: retryable error", resp.StatusCode)
			lastStatus = resp.StatusCode
			continue
		}

		return resp, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("request failed after %d attempts: %w", config.maxRetries+1, lastErr)
	}
	return nil, fmt.Errorf("request failed after %d attempts", config.maxRetries+1)
}
