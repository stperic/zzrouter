package prov_apps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// MaxWorkflowBytes bounds workflow and official catalog documents.
const MaxWorkflowBytes = 4 << 20

const workflowTemplateOrigin = "https://raw.githubusercontent.com/Comfy-Org/workflow_templates/main/templates/"

// WorkflowTemplate identifies one entry in the official ComfyUI catalog.
type WorkflowTemplate struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	Description       string `json:"description,omitempty"`
	MinComfyUIVersion string `json:"min_comfyui_version,omitempty"`
	OpenSource        *bool  `json:"open_source,omitempty"`
	Source            string `json:"source"`
	SHA256            string `json:"workflow_sha256,omitempty"`
}

// WorkflowTemplateCatalog reads official templates without executing their nodes.
type WorkflowTemplateCatalog struct{ client *http.Client }

// NewWorkflowTemplateCatalog owns a client copy that refuses redirects.
func NewWorkflowTemplateCatalog(client *http.Client) *WorkflowTemplateCatalog {
	owned := *client
	owned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &WorkflowTemplateCatalog{client: &owned}
}

// List returns a deduplicated catalog in canonical ID order.
func (c *WorkflowTemplateCatalog) List(ctx context.Context) ([]WorkflowTemplate, error) {
	raw, err := c.read(ctx, "index.json")
	if err != nil {
		return nil, err
	}
	var groups []struct {
		Templates []struct {
			Name        string `json:"name"`
			Title       string `json:"title"`
			Description string `json:"description"`
			MinVersion  string `json:"minComfyUIVersion"`
			OpenSource  *bool  `json:"openSource"`
		} `json:"templates"`
	}
	if json.Unmarshal(raw, &groups) != nil {
		return nil, templateSourceShape("invalid template catalog")
	}
	byID := map[string]WorkflowTemplate{}
	for _, group := range groups {
		for _, item := range group.Templates {
			if !validWorkflowTemplateID(item.Name) || strings.TrimSpace(item.Title) == "" {
				return nil, templateSourceShape("invalid template catalog entry")
			}
			entry := WorkflowTemplate{ID: item.Name, Title: item.Title, Description: item.Description, MinComfyUIVersion: item.MinVersion, OpenSource: item.OpenSource, Source: workflowTemplateOrigin + item.Name + ".json"}
			if old, ok := byID[entry.ID]; ok {
				sameSource := old.OpenSource == nil && entry.OpenSource == nil || old.OpenSource != nil && entry.OpenSource != nil && *old.OpenSource == *entry.OpenSource
				if old.Title != entry.Title || old.Description != entry.Description || old.MinComfyUIVersion != entry.MinComfyUIVersion || !sameSource {
					return nil, templateSourceShape("conflicting template catalog entries")
				}
			}
			byID[entry.ID] = entry
		}
	}
	if len(byID) == 0 {
		return nil, templateSourceShape("template catalog contains no entries")
	}
	entries := make([]WorkflowTemplate, 0, len(byID))
	for _, entry := range byID {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b WorkflowTemplate) int { return strings.Compare(a.ID, b.ID) })
	return entries, nil
}

// Resolve selects an exact ID or unique exact title and fetches its workflow.
func (c *WorkflowTemplateCatalog) Resolve(ctx context.Context, selector string) (*WorkflowTemplate, json.RawMessage, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeRequired), Message: "template selector is required"}
	}
	entries, err := c.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	matches := []WorkflowTemplate{}
	for _, entry := range entries {
		if entry.ID == selector {
			matches = []WorkflowTemplate{entry}
			break
		}
		if strings.EqualFold(entry.Title, selector) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		code, status := httperr.CodeTemplateNotFound, http.StatusNotFound
		detail := "unknown template; search the official template catalog"
		if len(matches) > 1 {
			code, status, detail = httperr.CodeTemplateAmbiguous, http.StatusConflict, "template title is ambiguous; choose a canonical template id"
		}
		return nil, nil, httperr.Error{Status: status, Code: string(code), Message: detail, Extra: map[string]any{"templates": matches[:min(len(matches), 10)]}}
	}
	entry := matches[0]
	if entry.OpenSource != nil && !*entry.OpenSource {
		return nil, nil, httperr.Error{Status: http.StatusBadRequest, Code: string(httperr.CodeTemplateUnsupported), Message: "API-only templates do not install local model files"}
	}
	raw, err := c.read(ctx, entry.ID+".json")
	if err != nil {
		return nil, nil, err
	}
	if !json.Valid(raw) || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return nil, nil, templateSourceShape("template workflow must be a JSON object")
	}
	entry.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	return &entry, raw, nil
}

func validWorkflowTemplateID(id string) bool {
	if id == "" {
		return false
	}
	for i, ch := range id {
		alnum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || ch != '_' && ch != '-' && ch != '.') {
			return false
		}
	}
	return true
}

func (c *WorkflowTemplateCatalog) read(ctx context.Context, name string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, workflowTemplateOrigin+name, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.client.Do(req)
	if err != nil {
		return nil, templateSourceFailure(err, "official template source did not respond")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, httperr.Error{Status: http.StatusBadGateway, Code: string(httperr.CodeTemplateSourceUnavailable), Message: "official template source returned an unsuccessful response"}
	}
	// Catalog and workflow documents share the existing workflow admission bound.
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxWorkflowBytes+1))
	if err != nil {
		return nil, templateSourceFailure(err, "official template source body could not be read")
	}
	if len(raw) > MaxWorkflowBytes {
		return nil, templateSourceShape("official template document exceeds 4 MiB")
	}
	return raw, nil
}

func templateSourceShape(detail string) error {
	return httperr.Error{Status: http.StatusBadGateway, Code: string(httperr.CodeUpstreamShape), Message: detail}
}

func templateSourceFailure(err error, detail string) error {
	code, status := httperr.CodeTemplateSourceUnavailable, http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		code, status = httperr.CodeUpstreamTimeout, http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		code = httperr.CodeUpstreamCanceled
	}
	return httperr.Error{Status: status, Code: string(code), Message: detail}
}
