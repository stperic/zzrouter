package huggingface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/modelregistry/metadata"
)

// ErrModelNotFound is a repository Hugging Face does not serve to this
// connector: absent, or gated behind a token it lacks.
var ErrModelNotFound = errors.New("not found on Hugging Face")

// GetRepoFiles fetches the list of files for a model from the
// HuggingFace API. Exported for the registries-discovery endpoint;
// internal callers should continue using getRepoFiles.
func (hfc *Connector) GetRepoFiles(ctx context.Context, modelID string) (metadata.RepoFiles, error) {
	return hfc.getRepoFiles(ctx, modelID)
}

// getRepoFiles fetches the list of files for a model from the
// HuggingFace API.
//
// Hits `/api/models/<modelID>?blobs=true` — the card API, with the
// blobs flag that adds per-sibling size metadata. Parses
// `siblings[].rfilename` + `siblings[].size` via generic map decoding
// so we do not need to import or depend on the search subpackage's
// DTO surface.
func (hfc *Connector) getRepoFiles(ctx context.Context, modelID string) (metadata.RepoFiles, error) {
	return hfc.getRepoFilesRevision(ctx, modelID, "main")
}

func (hfc *Connector) getRepoFilesRevision(ctx context.Context, modelID, revision string) (metadata.RepoFiles, error) {
	apiURL := fmt.Sprintf("%s%s?blobs=true", hfc.apiBase, modelID)
	if revision != "main" {
		apiURL = fmt.Sprintf("%s%s/revision/%s?blobs=true", hfc.apiBase, modelID, url.PathEscape(revision))
	}

	req, err := hfc.newGet(ctx, apiURL)
	if err != nil {
		return metadata.RepoFiles{}, err
	}
	client := hfc.httpClient(httpTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return metadata.RepoFiles{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 401 || resp.StatusCode == 404 {
			return metadata.RepoFiles{}, fmt.Errorf("model '%s' %w.\n\n💡 Tip: Use the full model ID format (organization/model-name)\n   Example: 'TinyLlama/TinyLlama-1.1B-Chat-v1.0'\n\nSearch for models: ./zzrouter model search \"%s\" --provider huggingface\n   Or visit: https://huggingface.co/models?search=%s", modelID, ErrModelNotFound, modelID, modelID)
		}
		return metadata.RepoFiles{}, fmt.Errorf("API returned status %d for model %s", resp.StatusCode, modelID)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return metadata.RepoFiles{}, fmt.Errorf("failed to read API response: %w", err)
	}
	return parseRepoFilesJSON(body, modelID)
}

// parseRepoFilesJSON extracts validated entries from the HF card-API
// JSON. Split from getRepoFiles for fuzz coverage — see
// tree_fuzz_test.go. Names failing validateRepoFileName are dropped
// (logged) rather than failing the whole listing.
func parseRepoFilesJSON(body []byte, modelID string) (metadata.RepoFiles, error) {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return metadata.RepoFiles{}, err
	}

	siblings, ok := data["siblings"].([]any)
	if !ok {
		return metadata.RepoFiles{}, fmt.Errorf("invalid API response format")
	}

	var listing metadata.RepoFiles
	if gguf, ok := data["gguf"].(map[string]any); ok {
		listing.Architecture, _ = gguf["architecture"].(string)
	}
	for _, sibling := range siblings {
		s, ok := sibling.(map[string]any)
		if !ok {
			continue
		}
		name, _ := s["rfilename"].(string)
		size, _ := s["size"].(float64)
		if name == "" {
			continue
		}
		if err := validateRepoFileName(name); err != nil {
			slog.Warn("modelregistry: dropping HF repo entry with invalid filename",
				"model", modelID, "name", name, "err", err)
			continue
		}
		// LFS oid is the sha256 of the file content; absent for plain repo
		// files (HF only hashes LFS objects).
		var sha string
		if lfs, ok := s["lfs"].(map[string]any); ok {
			sha, _ = lfs["oid"].(string)
		}
		listing.Files = append(listing.Files, metadata.TreeFileEntry{
			Name:   name,
			Size:   int64(size),
			SHA256: sha,
		})
	}

	return listing, nil
}
