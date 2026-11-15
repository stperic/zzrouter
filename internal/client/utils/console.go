package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/stperic/common/mcp/oauth"
)

// ConsoleTarget selects a screen in the web console. Item and Mode use the
// console's existing hash navigation, keeping credentials out of the URL.
type ConsoleTarget struct {
	Page string
	Item string
	Mode string
}

// ConsoleURL discovers the configured public coordinator origin. Discovery is
// read-only: unlike /session, OAuth metadata does not create a browser session.
func (c *Client) ConsoleURL(ctx context.Context, target ConsoleTarget) (string, error) {
	var metadata struct {
		Issuer string `json:"issuer"`
	}
	if err := c.doContextJSON(ctx, http.MethodGet, "/.well-known/oauth-authorization-server", nil, &metadata, "discover web console"); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMisdirectedRequest) {
			return "", fmt.Errorf("web console discovery is unavailable: connect to the coordinator and enable OAuth for browser access: %w", err)
		}
		return "", err
	}
	return consoleTargetURL(metadata.Issuer, target)
}

func consoleTargetURL(issuer string, target ConsoleTarget) (string, error) {
	// Match the coordinator's validation rather than inventing another origin
	// policy. This also refuses browser-normalized host and port spellings.
	if err := oauth.ValidateIssuer(issuer); err != nil {
		return "", fmt.Errorf("coordinator returned an invalid public console origin: %w", err)
	}
	if target.Page == "" {
		target.Page = "overview"
	}
	switch target.Page {
	case "overview", "chat", "models", "nodes", "workloads", "logs", "clients", "providers", "routes", "access", "connections", "settings":
	default:
		return "", fmt.Errorf("unsupported console screen %q", target.Page)
	}
	if target.Mode != "" && target.Mode != "create" {
		return "", fmt.Errorf("unsupported console action %q", target.Mode)
	}
	query := url.Values{}
	if target.Item != "" {
		query.Set("item", target.Item)
	}
	if target.Mode != "" {
		query.Set("mode", target.Mode)
	}
	// The hash has its own encoded query. Append it after serializing the origin
	// so URL.String does not encode its percent escapes a second time.
	fragment := target.Page
	if encoded := query.Encode(); encoded != "" {
		fragment += "?" + encoded
	}
	return issuer + "/console/#" + fragment, nil
}
