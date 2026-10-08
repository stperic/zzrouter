package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// huggingFaceAPIBase is the HF model-card endpoint prefix. Only
// getHuggingFaceModelCard uses it; kept local to this file rather than
// globally exported.
const huggingFaceAPIBase = "https://huggingface.co/api/models/"

// ModelCard represents a model's card/information.
type ModelCard struct {
	Provider    Provider    `json:"provider"`
	ModelID     string      `json:"model_id"`
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	README      string      `json:"readme,omitempty"`
	Tags        []string    `json:"tags,omitempty"`
	License     string      `json:"license,omitempty"`
	Author      string      `json:"author,omitempty"`
	Size        int64       `json:"size,omitempty"` // Size in bytes
	PipelineTag string      `json:"pipeline_tag,omitempty"`
	Downloads   int         `json:"downloads,omitempty"`
	Likes       int         `json:"likes,omitempty"`
	CreatedAt   string      `json:"created_at,omitempty"`
	UpdatedAt   string      `json:"updated_at,omitempty"`
	URL         string      `json:"url,omitempty"`      // Model page URL
	Siblings    []hfSibling `json:"siblings,omitempty"` // Files in the repository
}

// GetModelCard retrieves the model card for a specific model by ID,
// dispatching to the per-provider card fetcher and caching the result.
func GetModelCard(provider Provider, modelID string) (*ModelCard, error) {
	if cached, exists := getCachedModelCard(provider, modelID); exists {
		return cached, nil
	}

	var card *ModelCard
	var err error

	switch provider {
	case ProviderHuggingFace:
		card, err = getHuggingFaceModelCard(modelID)
	case ProviderOllama:
		card, err = getOllamaModelCard(modelID)
	default:
		if IsCloudSearchProvider(provider) {
			card, err = getCloudModelCard(provider, modelID)
		} else {
			return nil, fmt.Errorf("unsupported provider: %s", provider.String())
		}
	}

	if err != nil {
		return nil, err
	}

	setCachedModelCard(provider, modelID, card)
	return card, nil
}

// GetCloudModelRaw returns the raw provider response for a cloud model as a
// map, preserving provider-specific fields (Anthropic capabilities,
// created_at, etc.) that the ModelCard struct drops.
func GetCloudModelRaw(provider Provider, modelID string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results, err := SearchCloudProvider(ctx, provider, modelID, 100)
	if err != nil {
		return nil, fmt.Errorf("cloud model lookup failed: %w", err)
	}

	NormalizeCloudResults(results)

	for _, item := range results {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		apiID, _ := m["api_id"].(string)
		if apiID == modelID {
			if entry, ok := GetCloudProvider(provider); ok && entry.Meta.ModelURLFmt != "" {
				m["url"] = fmt.Sprintf(entry.Meta.ModelURLFmt, apiID)
			}
			return m, nil
		}
	}

	return nil, fmt.Errorf("model %s not found in %s", modelID, provider)
}

// getCloudModelCard builds a ModelCard from the raw cloud-provider response.
// Callers wanting full provider detail should use GetCloudModelRaw.
func getCloudModelCard(provider Provider, modelID string) (*ModelCard, error) {
	raw, err := GetCloudModelRaw(provider, modelID)
	if err != nil {
		return nil, err
	}

	card := &ModelCard{
		Provider: provider,
		ModelID:  modelID,
		Name:     modelID,
	}
	if v, _ := raw["owned_by"].(string); v != "" {
		card.Author = v
	}
	if v, _ := raw["display_name"].(string); v != "" {
		card.Name = v
	}
	if v, _ := raw["description"].(string); v != "" {
		card.Description = v
	}
	if v, ok := raw["created"].(float64); ok && v > 0 {
		card.CreatedAt = time.Unix(int64(v), 0).Format(time.RFC3339)
	}
	if v, _ := raw["created_at"].(string); v != "" {
		card.CreatedAt = v
	}
	if v, _ := raw["url"].(string); v != "" {
		card.URL = v
	}

	return card, nil
}

// getHuggingFaceModelCard retrieves a model card from Hugging Face Hub.
func getHuggingFaceModelCard(modelID string) (*ModelCard, error) {
	client := NewDefaultClient()
	ctx := context.Background()

	req, err := http.NewRequestWithContext(ctx, "GET", huggingFaceAPIBase+modelID+"?full=true", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if client.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+client.apiToken)
	}

	resp, err := doRequestWithRetry(ctx, client.httpClient, req, defaultRetryConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch model info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hugging Face API returned status %d for model %s", resp.StatusCode, modelID)
	}

	var hfModel ModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&hfModel); err != nil {
		return nil, fmt.Errorf("failed to parse model info: %w", err)
	}

	// README is best-effort; a failure to fetch it is not fatal.
	var readmeContent string
	readmeReq, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://huggingface.co/%s/resolve/main/README.md", modelID), nil)
	if err == nil {
		if client.apiToken != "" {
			readmeReq.Header.Set("Authorization", "Bearer "+client.apiToken)
		}
		readmeResp, err := doRequestWithRetry(ctx, client.httpClient, readmeReq, defaultRetryConfig())
		if err == nil && readmeResp.StatusCode == http.StatusOK {
			readmeBytes, err := io.ReadAll(readmeResp.Body)
			_ = readmeResp.Body.Close()
			if err == nil {
				readmeContent = string(readmeBytes)
			}
		} else if readmeResp != nil {
			_ = readmeResp.Body.Close()
		}
	}

	description, capabilities := extractHFDescriptionAndCapabilities(readmeContent)

	// Metadata from CardData first; YAML frontmatter is the fallback.
	var license string
	var yamlTags []string
	if hfModel.CardData != nil {
		if lic, ok := hfModel.CardData["license"].(string); ok {
			license = lic
		}
		if tags, ok := hfModel.CardData["tags"].([]any); ok {
			for _, tag := range tags {
				if tagStr, ok := tag.(string); ok {
					yamlTags = append(yamlTags, tagStr)
				}
			}
		}
	}
	if license == "" || len(yamlTags) == 0 {
		yamlData := parseYAMLFrontmatter(readmeContent)
		if yamlData != nil {
			if lic, ok := yamlData["license"].(string); ok && license == "" {
				license = lic
			}
			if tags, ok := yamlData["tags"].([]any); ok && len(yamlTags) == 0 {
				for _, tag := range tags {
					if tagStr, ok := tag.(string); ok {
						yamlTags = append(yamlTags, tagStr)
					}
				}
			}
		}
	}

	author := ""
	if parts := strings.Split(modelID, "/"); len(parts) == 2 {
		author = parts[0]
	}

	allTags := hfModel.Tags
	allTags = append(allTags, yamlTags...)
	allTags = append(allTags, capabilities...)

	var totalSize int64
	if hfModel.Siblings != nil {
		for _, sibling := range hfModel.Siblings {
			totalSize += int64(sibling.Size)
		}
	}

	modelCard := &ModelCard{
		Provider:    ProviderHuggingFace,
		ModelID:     modelID,
		Name:        hfModel.ID,
		Description: description,
		README:      readmeContent,
		Tags:        allTags,
		License:     license,
		Author:      author,
		Size:        totalSize,
		PipelineTag: hfModel.PipelineTag,
		Downloads:   hfModel.Downloads,
		Likes:       hfModel.Likes,
		CreatedAt:   hfModel.CreatedAt,
		Siblings:    hfModel.Siblings,
	}

	validateModelCard(modelCard)
	return modelCard, nil
}

// getOllamaModelCard retrieves a model card from the Ollama registry.
// Tagged names ("qwen2.5:0.5b", "all-minilm:latest") match the bare family
// in the library index — strip the suffix before lookup, but pass the
// original (untagged) base to the scraper since ollama.com/library only
// hosts family pages, not per-tag pages.
func getOllamaModelCard(modelName string) (*ModelCard, error) {
	models, err := GetOllamaModels()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Ollama models: %w", err)
	}

	baseName := modelName
	if i := strings.IndexByte(baseName, ':'); i >= 0 {
		baseName = baseName[:i]
	}

	var foundModel *OllamaModelInfo
	for _, model := range models {
		if strings.EqualFold(model.Name, baseName) {
			foundModel = &model
			break
		}
	}
	if foundModel == nil {
		return nil, fmt.Errorf("model %s not found in Ollama registry", modelName)
	}

	description, capabilities, _, modelURL := scrapeOllamaModelPage(baseName)

	return &ModelCard{
		Provider:    ProviderOllama,
		ModelID:     modelName,
		Name:        foundModel.Name,
		Description: description,
		Size:        foundModel.Size,
		Downloads:   foundModel.Pulls,
		Tags:        capabilities,
		URL:         modelURL,
	}, nil
}

// scrapeOllamaModelPage fetches the Ollama library page and pulls out the
// description, capability badges, inferred input types, and canonical URL.
// Best-effort — returns empty values if anything goes wrong.
func scrapeOllamaModelPage(modelName string) (description string, capabilities []string, inputTypes []string, modelURL string) {
	url := fmt.Sprintf("https://ollama.com/library/%s", modelName)
	client := &http.Client{Timeout: httpTimeout}

	resp, err := client.Get(url)
	if err != nil {
		return "", nil, nil, ""
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", nil, nil, ""
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, nil, ""
	}

	htmlContent := string(body)

	if matches := regexp.MustCompile(`<meta name="description" content="([^"]*)"`).FindStringSubmatch(htmlContent); len(matches) > 1 {
		description = matches[1]
	}

	if matches := regexp.MustCompile(`<meta property="og:url" content="([^"]*)"`).FindStringSubmatch(htmlContent); len(matches) > 1 {
		modelURL = matches[1]
		if !strings.Contains(modelURL, "/library/") {
			modelURL = fmt.Sprintf("https://ollama.com/library/%s", modelName)
		}
	} else {
		modelURL = fmt.Sprintf("https://ollama.com/library/%s", modelName)
	}

	matches := regexp.MustCompile(`<span[^>]*>tools</span>|<span[^>]*>thinking</span>`).FindAllString(htmlContent, -1)
	for _, match := range matches {
		if strings.Contains(match, "tools") {
			capabilities = append(capabilities, "tools")
		}
		if strings.Contains(match, "thinking") {
			capabilities = append(capabilities, "thinking")
		}
	}

	content := strings.ToLower(description)
	inputTypes = append(inputTypes, "text")
	if strings.Contains(content, "multimodal") ||
		strings.Contains(content, "vision") ||
		strings.Contains(content, "image") ||
		strings.Contains(content, "visual") ||
		strings.Contains(content, "llava") ||
		modelName == "llava" {
		inputTypes = append(inputTypes, "image")
	}

	return description, capabilities, inputTypes, modelURL
}

// extractHFDescriptionAndCapabilities scrapes description and capability
// keywords from a HuggingFace README body.
//
//nolint:gocyclo,cyclop // keyword-matching pass with one branch per capability category
func extractHFDescriptionAndCapabilities(readmeContent string) (description string, capabilities []string) {
	if readmeContent == "" {
		return "", nil
	}

	content := strings.ToLower(readmeContent)
	if strings.Contains(content, "tool") || strings.Contains(content, "function call") {
		capabilities = append(capabilities, "tools")
	}
	if strings.Contains(content, "think") || strings.Contains(content, "reason") || strings.Contains(content, "chain of thought") {
		capabilities = append(capabilities, "thinking")
	}

	lines := strings.Split(readmeContent, "\n")
	var descriptionLines []string
	inFrontmatter := false
	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "---" {
			inFrontmatter = !inFrontmatter
			continue
		}
		if inFrontmatter {
			continue
		}

		if strings.HasPrefix(line, "<!--") && strings.HasSuffix(line, "-->") {
			continue
		}
		if strings.HasPrefix(line, "<!--") {
			continue
		}
		if strings.HasSuffix(line, "-->") {
			continue
		}

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") {
			continue
		}
		if strings.Contains(line, "<img") || strings.Contains(line, "<div") || strings.Contains(line, "<hr") {
			continue
		}

		if strings.HasPrefix(line, "#") {
			if strings.HasPrefix(line, "##") || strings.HasPrefix(line, "###") {
				continue
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		}

		if strings.Contains(line, "shields.io") || strings.Contains(line, "badge") {
			continue
		}

		if strings.HasPrefix(line, "##") ||
			strings.HasPrefix(line, "```") ||
			strings.Contains(strings.ToLower(line), "usage") ||
			strings.Contains(strings.ToLower(line), "example") ||
			strings.Contains(strings.ToLower(line), "installation") ||
			strings.Contains(strings.ToLower(line), "quick start") ||
			strings.Contains(strings.ToLower(line), "getting started") {
			break
		}

		if len(line) < 10 && (strings.Contains(line, "http") || !strings.Contains(line, " ")) {
			continue
		}

		if len(descriptionLines) < 20 {
			descriptionLines = append(descriptionLines, line)
		}
	}

	description = strings.Join(descriptionLines, " ")
	description = strings.TrimSpace(description)
	description = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`).ReplaceAllString(description, "$1")
	description = regexp.MustCompile(`\s+`).ReplaceAllString(description, " ")

	if len(description) > 10000 {
		description = description[:10000] + "..."
	}

	return description, capabilities
}

// validateModelCard sanitizes a ModelCard in place: trims/dedupes tags and
// enforces size limits on description + README so a pathological upstream
// response can't balloon memory.
func validateModelCard(card *ModelCard) {
	if card == nil {
		return
	}

	// Tag cleanup: trim, drop overly long tags, dedupe preserving order.
	var cleanTags []string
	for _, tag := range card.Tags {
		tag = strings.TrimSpace(tag)
		if tag != "" && len(tag) <= 100 {
			cleanTags = append(cleanTags, tag)
		}
	}
	seen := make(map[string]bool)
	var uniqueTags []string
	for _, tag := range cleanTags {
		if !seen[tag] {
			seen[tag] = true
			uniqueTags = append(uniqueTags, tag)
		}
	}
	card.Tags = uniqueTags

	// Description + README size caps.
	if len(card.Description) > 10000 {
		card.Description = card.Description[:10000] + "..."
	}
	if len(card.README) > 1000000 {
		card.README = card.README[:1000000] + "\n\n[README truncated due to size]"
	}
}

// parseYAMLFrontmatter extracts and parses YAML frontmatter from a markdown
// body. Handles the standard "---" delimiter form; falls back to a best-
// effort scan when the closing delimiter is missing.
func parseYAMLFrontmatter(content string) map[string]any {
	if content == "" {
		return nil
	}

	lines := strings.Split(content, "\n")
	var yamlLines []string
	inFrontmatter := false
	frontmatterStart := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontmatter {
				inFrontmatter = true
				frontmatterStart = i
				continue
			}
			yamlLines = lines[frontmatterStart+1 : i]
			break
		}
	}

	// Fallback: frontmatter without a closing "---"
	if len(yamlLines) == 0 && inFrontmatter && frontmatterStart >= 0 {
		for i := frontmatterStart + 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, ":") && !strings.HasPrefix(line, "-") {
				yamlLines = lines[frontmatterStart+1 : i]
				break
			}
		}
		if len(yamlLines) == 0 {
			yamlLines = lines[frontmatterStart+1:]
		}
	}

	if len(yamlLines) == 0 {
		return nil
	}

	yamlContent := strings.Join(yamlLines, "\n")
	var data map[string]any
	if err := yaml.Unmarshal([]byte(yamlContent), &data); err != nil {
		yamlContent = strings.TrimSpace(yamlContent)
		if yamlContent == "" {
			return nil
		}
		if err2 := yaml.Unmarshal([]byte(yamlContent), &data); err2 != nil {
			return nil
		}
	}

	return data
}
