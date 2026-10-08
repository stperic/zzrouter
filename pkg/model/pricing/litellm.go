package pricing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stperic/zzrouter/pkg/utils"
)

// fetchResult holds the result of a fetch operation.
type fetchResult struct {
	Data     map[string]ModelPricing
	Metadata CacheMetadata
	Changed  bool // false if server returned 304 Not Modified
}

// fetch downloads pricing data from the given URL.
// Uses conditional requests (ETag / If-Modified-Since) to avoid
// re-downloading unchanged data.
func fetch(ctx context.Context, url string, prev *CacheMetadata) (*fetchResult, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "zzrouter/pricing")

	// Conditional request headers
	if prev != nil {
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		}
		if prev.LastModified != "" {
			req.Header.Set("If-Modified-Since", prev.LastModified)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch pricing data: %w", err)
	}
	defer resp.Body.Close()

	// Not modified — data hasn't changed
	if resp.StatusCode == http.StatusNotModified {
		return &fetchResult{Changed: false}, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	// Cap body size to prevent OOM from a malicious or misconfigured upstream.
	// Current LiteLLM payload is ~1MB; 50MB is generous headroom.
	const maxBodySize = 50 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) >= maxBodySize {
		return nil, fmt.Errorf("pricing response exceeds %d bytes, aborting", maxBodySize)
	}

	// Check content hash to detect changes even without ETag/304 support
	hash := fmt.Sprintf("%x", sha256.Sum256(body))
	if prev != nil && prev.ContentHash == hash {
		return &fetchResult{Changed: false}, nil
	}

	data, err := parseLiteLLM(body)
	if err != nil {
		return nil, err
	}

	meta := CacheMetadata{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentHash:  hash,
		FetchedAt:    utils.Now(),
		ModelCount:   len(data),
	}

	slog.Info("Fetched LiteLLM pricing data", "models", len(data), "hash", hash[:12])

	return &fetchResult{
		Data:     data,
		Metadata: meta,
		Changed:  true,
	}, nil
}

// parseLiteLLM parses the raw LiteLLM JSON into our ModelPricing map.
// The JSON is a flat object: { "model-name": { ...fields... }, ... }
// The first entry "sample_spec" is a template and is skipped.
func parseLiteLLM(raw []byte) (map[string]ModelPricing, error) {
	var rawModels map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawModels); err != nil {
		return nil, fmt.Errorf("parse LiteLLM JSON: %w", err)
	}

	result := make(map[string]ModelPricing, len(rawModels))
	for name, rawEntry := range rawModels {
		if name == "sample_spec" {
			continue
		}

		var pricing ModelPricing
		if err := json.Unmarshal(rawEntry, &pricing); err != nil {
			slog.Debug("Skipping unparseable model", "model", name, "error", err)
			continue
		}

		// Lowercase keys for case-insensitive lookups
		result[strings.ToLower(name)] = pricing
	}

	return result, nil
}
