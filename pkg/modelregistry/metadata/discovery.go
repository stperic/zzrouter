package metadata

import (
	"encoding/json"
	"fmt"
)

// GGUFVariant is one loadable set of GGUF weights in a HuggingFace repo: a
// single file, or every shard of a split one, grouped the way downloads and
// launches group them. `File` is the exact string to pass as a deployment's
// `file`; it fetches every shard.
type GGUFVariant struct {
	File                 string `json:"file"`
	Shards               int    `json:"shards"`
	Quantization         string `json:"quantization"`
	Bits                 int    `json:"bits"`
	SizeBytes            int64  `json:"size_bytes"`
	SizeHuman            string `json:"size_human"`
	Recommended          bool   `json:"recommended,omitempty"`
	RecommendationReason string `json:"recommendation_reason,omitempty"`
}

// FeatureFile is the file a deployment requesting Feature would fetch beside
// the weights (a vision projector, say). It is never a weights choice.
type FeatureFile struct {
	Feature   string `json:"feature"`
	File      string `json:"file"`
	SizeBytes int64  `json:"size_bytes"`
	SizeHuman string `json:"size_human"`
}

// HuggingFaceVariantsResponse is the response envelope. Counts (not lists)
// for non-GGUF files keeps the payload focused on the decision-making
// surface while still signalling "this repo has docs/configs too".
type HuggingFaceVariantsResponse struct {
	ModelID        string        `json:"model_id"`
	Variants       []GGUFVariant `json:"variants"`
	Features       []FeatureFile `json:"features"`
	NonGGUFFiles   int           `json:"non_gguf_files"`
	TotalSizeBytes int64         `json:"total_size_bytes"`
}

// OllamaTag describes one catalog version. IsCloud is derived by the registry.
type OllamaTag struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	Context string `json:"context"`
	Input   string `json:"input"`
	Digest  string `json:"digest"`
	IsCloud bool   `json:"is_cloud"`
}

// OllamaTags is the public catalog tag response.
type OllamaTags struct {
	ModelID string      `json:"model_id"`
	Tags    []OllamaTag `json:"tags"`
}

// Discovery describes a model independently of its provider's native payload.
// SizeBytes is storage bytes when known; ParameterCount is never a byte size.
type Discovery struct {
	Provider       string   `json:"provider"`
	APIID          string   `json:"api_id"`
	DisplayName    string   `json:"display_name"`
	Description    string   `json:"description,omitempty"`
	README         string   `json:"readme,omitempty"`
	Downloads      int      `json:"downloads,omitempty"`
	Likes          int      `json:"likes,omitempty"`
	TrendingScore  float64  `json:"trending_score,omitempty"`
	CreatedAt      string   `json:"created_at,omitempty"`
	ModifiedAt     string   `json:"modified_at,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	SizeBytes      int64    `json:"size_bytes,omitempty"`
	ParameterCount int64    `json:"parameter_count,omitempty"`
	URL            string   `json:"url,omitempty"`
	VariantCount   int      `json:"variant_count,omitempty"`
	ContextLength  int      `json:"context_length,omitempty"`
	PricePrompt    string   `json:"price_prompt,omitempty"`
	PriceComplete  string   `json:"price_complete,omitempty"`
	HasCloud       bool     `json:"has_cloud,omitempty"`
	IsCloud        bool     `json:"is_cloud,omitempty"`
}

// DiscoveryResult retains native fields alongside authoritative discovery metadata.
// Native contains the original fields, including provider-specific capabilities.
type DiscoveryResult struct {
	Discovery Discovery      `json:"discovery"`
	Native    map[string]any `json:"-"`
}

// MarshalJSON adds discovery metadata without discarding native provider fields.
func (r DiscoveryResult) MarshalJSON() ([]byte, error) {
	fields := make(map[string]any, len(r.Native)+1)
	for k, v := range r.Native {
		fields[k] = v
	}
	fields["discovery"] = r.Discovery
	return json.Marshal(fields)
}

// UnmarshalJSON decodes typed metadata and retains every native provider field.
func (r *DiscoveryResult) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	value, ok := fields["discovery"]
	if !ok {
		return fmt.Errorf("server response is missing discovery metadata; update the server")
	}
	var discovery Discovery
	if err := json.Unmarshal(value, &discovery); err != nil {
		return err
	}
	var native map[string]any
	if err := json.Unmarshal(data, &native); err != nil {
		return err
	}
	delete(native, "discovery")
	r.Discovery, r.Native = discovery, native
	return nil
}

// SearchQuery specifies one model search.
type SearchQuery struct {
	Query, Provider, Sort, Order, Tags, Logic, Author, Cursor string
	Limit, Offset                                             int
	Full, Enrich                                              bool
}

// SearchPagination preserves the upstream pagination evidence.
type SearchPagination struct {
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
	HasNext bool          `json:"has_next"`
	HasPrev bool          `json:"has_prev"`
	Cursor  *SearchCursor `json:"cursor,omitempty"`
}

// SearchPage is the typed public model search payload.
type SearchPage struct {
	Type         string            `json:"type"`
	Query        string            `json:"query"`
	Provider     string            `json:"provider"`
	Results      []DiscoveryResult `json:"results"`
	Total        int               `json:"total"`
	IsTotalExact bool              `json:"is_total_exact"`
	Pagination   SearchPagination  `json:"pagination"`
}

// SearchProvider declares the server-supported discovery presentation options.
type SearchProvider struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Description  string   `json:"description"`
	BaseURL      string   `json:"base_url"`
	ModelURLFmt  string   `json:"model_url_fmt"`
	Icon         string   `json:"icon"`
	SortOptions  []string `json:"sort_options"`
	ClientSorts  []string `json:"client_sorts"`
	ClientFilter bool     `json:"client_filter"`
	HasTags      bool     `json:"has_tags"`
	IsCloud      bool     `json:"is_cloud"`
	Configured   bool     `json:"configured"`
	Enabled      bool     `json:"enabled"`
}

// SearchCursor carries opaque upstream page cursors without reconstructing URLs.
type SearchCursor struct {
	Next     string `json:"next,omitempty"`
	Previous string `json:"prev,omitempty"`
	First    string `json:"first,omitempty"`
	Last     string `json:"last,omitempty"`
}
