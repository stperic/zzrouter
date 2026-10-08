package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// GGUFVariant mirrors the server's HuggingFaceVariantsResponse entry.
type GGUFVariant struct {
	File                 string `json:"file"`
	Quantization         string `json:"quantization"`
	Bits                 int    `json:"bits"`
	SizeBytes            int64  `json:"size_bytes"`
	SizeHuman            string `json:"size_human"`
	Recommended          bool   `json:"recommended,omitempty"`
	RecommendationReason string `json:"recommendation_reason,omitempty"`
}

// HuggingFaceVariantsResponse mirrors the server's response envelope.
type HuggingFaceVariantsResponse struct {
	ModelID        string        `json:"model_id"`
	Variants       []GGUFVariant `json:"variants"`
	NonGGUFFiles   int           `json:"non_gguf_files"`
	TotalSizeBytes int64         `json:"total_size_bytes"`
}

// GetHuggingFaceVariants fetches the GGUF variant catalog for a HuggingFace repo.
func (c *Client) GetHuggingFaceVariants(modelID string) (*HuggingFaceVariantsResponse, error) {
	path := apipath.RegistriesHuggingFaceVariants + "?id=" + url.QueryEscape(modelID)

	resp, err := c.makeRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get variants failed with status %d: %s", resp.StatusCode, parseErrorResponse(body))
	}

	var out HuggingFaceVariantsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &out, nil
}
