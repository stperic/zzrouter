package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenRouterFreeModel is the subset of search-result fields we use to
// pick a free-tier model at runtime.
type OpenRouterFreeModel struct {
	ID      string  `json:"id"`
	Name    string  `json:"name,omitempty"`
	Pricing Pricing `json:"pricing,omitempty"`
}

// Pricing mirrors the openrouter pricing block.
type Pricing struct {
	Prompt     string `json:"prompt,omitempty"`
	Completion string `json:"completion,omitempty"`
}

// ListOpenRouterFreeModels queries the search surface for OpenRouter
// models matching "free" and returns IDs with both prompt and completion
// price = "0", filtered to families that produce plain chat output (no
// reasoning/thinking/multimodal/OCR variants — they emit content under
// alternate response fields and break structural assertions).
//
// The :free tier rotates as upstreams come and go, so callers resolve
// at test time rather than pin specific ids in fixtures. The returned
// slice preserves the search service's ranking, so the most-trending
// model is first.
//
// Caller must register a deployment via POST /zzrouter/v1/deployments
// for any chosen id before using it at /v1/chat/completions.
func ListOpenRouterFreeModels(ctx context.Context, c *Client) ([]string, error) {
	resp, err := c.GET(ctx, "/zzrouter/v1/search?provider=openrouter&q=free&type=models&limit=30")
	if err != nil {
		return nil, fmt.Errorf("search openrouter free: %w", err)
	}
	if resp.Status != 200 {
		return nil, fmt.Errorf("search openrouter free: status %d body=%s", resp.Status, resp.Body)
	}
	var env struct {
		Data struct {
			Results []OpenRouterFreeModel `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}
	var out []string
	for _, m := range env.Data.Results {
		if !strings.HasSuffix(m.ID, ":free") {
			continue
		}
		if m.Pricing.Prompt != "0" || m.Pricing.Completion != "0" {
			continue
		}
		lower := strings.ToLower(m.ID)
		if strings.Contains(lower, "reasoning") ||
			strings.Contains(lower, "thinking") ||
			strings.Contains(lower, "omni") ||
			strings.Contains(lower, "ocr") ||
			strings.Contains(lower, "vision") ||
			strings.Contains(lower, "embed") {
			continue
		}
		out = append(out, m.ID)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no free-tier openrouter chat models found in %d results", len(env.Data.Results))
	}
	return out, nil
}
