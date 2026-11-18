package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/stperic/common/httpclient"
	"golang.org/x/net/html"
)

var openRouterModelSlug = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*/[a-zA-Z0-9][a-zA-Z0-9._-]*(?::[a-zA-Z0-9][a-zA-Z0-9._-]*)?$`)

func expandOpenRouterDescription(ctx context.Context, endpoint, modelID, preview string) string {
	trimmed := strings.TrimSpace(preview)
	if (!strings.HasSuffix(trimmed, "...") && !strings.HasSuffix(trimmed, "…")) || !openRouterModelSlug.MatchString(modelID) {
		return preview
	}
	prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(trimmed, "..."), "…"))
	if prefix == "" {
		return preview
	}
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Scheme != "https" || (origin.Host != "openrouter.ai" && origin.Host != "eu.openrouter.ai") {
		return preview
	}

	// Use the public model page without forwarding the configured connection's credentials.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/"+modelID, nil)
	if err != nil {
		return preview
	}
	client := httpclient.SameOrigin(&http.Client{Timeout: 5 * time.Second})
	resp, err := client.Do(req)
	if err != nil {
		return preview
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return preview
	}
	if description := openRouterPageDescription(io.LimitReader(resp.Body, 2<<20), resp.Request.URL, prefix); description != "" {
		return description
	}
	return preview
}

func openRouterPageDescription(reader io.Reader, page *url.URL, prefix string) string {
	tokens := html.NewTokenizer(reader)
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			tag := tokens.Token()
			if tag.Data != "script" || !isJSONLD(tag.Attr) || tokens.Next() != html.TextToken {
				continue
			}
			var app struct {
				Type        string `json:"@type"`
				URL         string `json:"url"`
				Description string `json:"description"`
			}
			if json.Unmarshal(tokens.Text(), &app) != nil || app.Type != "SoftwareApplication" {
				continue
			}
			modelURL, err := url.Parse(app.URL)
			if err != nil || modelURL.Scheme != page.Scheme || modelURL.Host != page.Host ||
				strings.TrimRight(modelURL.Path, "/") != strings.TrimRight(page.Path, "/") {
				continue
			}
			description := strings.TrimSpace(app.Description)
			if len(description) > len(prefix) && strings.HasPrefix(description, prefix) {
				return description
			}
		}
	}
}

func isJSONLD(attrs []html.Attribute) bool {
	for _, attr := range attrs {
		if attr.Key == "type" && attr.Val == "application/ld+json" {
			return true
		}
	}
	return false
}
