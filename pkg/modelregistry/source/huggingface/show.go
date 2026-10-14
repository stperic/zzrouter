package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/httperr"
)

// ShowModel returns detailed information about a HuggingFace model in
// an Ollama-compatible structure. Prefers local config.json when the
// model is already on disk; falls back to the HF API otherwise.
func (hfc *Connector) ShowModel(ctx context.Context, modelID string) (map[string]any, error) {
	result := make(map[string]any)

	modelPath := filepath.Join(hfc.modelsDir, modelID)
	configPath := filepath.Join(modelPath, "config.json")

	var config map[string]any
	localConfigExists := false

	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &config); err == nil {
			localConfigExists = true
		}
	}

	if localConfigExists {
		extra := parseHuggingFaceConfig(configPath)
		maps.Copy(result, extra)

		// model_info section: Ollama-compatible dotted-key format with
		// only the simple-valued fields from config.json.
		modelInfo := make(map[string]any)
		for key, val := range config {
			switch v := val.(type) {
			case string, float64, bool, int, int64:
				modelInfo[key] = v
			}
		}
		result["model_info"] = modelInfo

		if info, err := os.Stat(filepath.Join(modelPath, "chat_template.jinja")); err == nil && info.Mode().IsRegular() {
			result["template_present"] = info.Size() > 0
		}
		for _, filename := range []string{"generation_config.json", "tokenizer_config.json"} {
			data, err := os.ReadFile(filepath.Join(modelPath, filename))
			var value map[string]any
			if err != nil || json.Unmarshal(data, &value) != nil || value == nil {
				continue
			}
			if filename == "generation_config.json" {
				result["parameters"] = value
			} else if template, reported := value["chat_template"].(string); reported {
				if _, known := result["template_present"]; !known {
					result["template_present"] = strings.TrimSpace(template) != ""
				}
			}
		}

	} else {
		apiURL := fmt.Sprintf("%s%s", hfc.apiBase, modelID)
		req, err := hfc.newGet(ctx, apiURL)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		client := hfc.httpClient(constants.HuggingFaceAPITimeout)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch model info from HuggingFace: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			return nil, httperr.Error{Status: resp.StatusCode, Message: fmt.Sprintf("HuggingFace model metadata returned HTTP %d", resp.StatusCode)}
		}

		body, err := io.ReadAll(resp.Body)
		var apiData map[string]any
		if err != nil || json.Unmarshal(body, &apiData) != nil || apiData == nil {
			return nil, httperr.Error{Status: http.StatusBadGateway, Code: string(httperr.CodeUpstreamShape), Message: "HuggingFace model metadata must be one JSON object"}
		}

		details := make(map[string]any)
		if pipelineTag, ok := apiData["pipeline_tag"].(string); ok {
			details["task"] = pipelineTag
		}
		result["details"] = details

		result["huggingface"] = map[string]any{
			"downloads": apiData["downloads"],
			"likes":     apiData["likes"],
			"tags":      apiData["tags"],
		}
	}

	// Pull license + full README model card from README.md when available.
	readmePath := filepath.Join(modelPath, "README.md")
	if readmeData, err := os.ReadFile(readmePath); err == nil {
		if cardInfo := parseModelCard(string(readmeData)); cardInfo != nil {
			if license, ok := cardInfo["license"].(string); ok {
				result["license"] = license
			}
			result["huggingface"] = map[string]any{
				"model_card": cardInfo,
				"readme":     string(readmeData),
			}
		}
	}

	return result, nil
}

// parseModelCard extracts YAML frontmatter from a README.md body and
// returns model-card metadata (license, language, tags, etc.). Uses a
// simple key:value parser — good enough for the canonical HF card shape,
// without pulling a full YAML dependency into the connector.
func parseModelCard(readme string) map[string]any {
	lines := strings.Split(readme, "\n")
	if len(lines) < 3 || lines[0] != "---" {
		return nil
	}

	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return nil
	}

	cardInfo := make(map[string]any)
	for i := 1; i < endIdx; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			// Inline lists like "tags: [a, b, c]".
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				value = strings.Trim(value, "[]")
				items := strings.Split(value, ",")
				var cleanItems []string
				for _, item := range items {
					cleanItems = append(cleanItems, strings.TrimSpace(item))
				}
				cardInfo[key] = cleanItems
			} else {
				cardInfo[key] = value
			}
		}
	}

	return cardInfo
}
