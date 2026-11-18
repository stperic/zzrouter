package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/stperic/common/mcp/oauth"
)

// ConsoleGrant transfers existing authority through a temporary, single-use code.
// Token is intentionally separate from URL and must not be logged or persisted.
type ConsoleGrant struct {
	Token     string    `json:"token"`
	Purpose   string    `json:"purpose"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BeginConsoleHandoff is called only on an explicit open/sign-in action.
// ConsoleURL remains the read-only discovery operation.
func (c *Client) BeginConsoleHandoff(ctx context.Context, target ConsoleTarget) (*ConsoleGrant, error) {
	var response struct {
		Data ConsoleGrant `json:"data"`
	}
	if err := c.doContextJSON(ctx, http.MethodPost, "/zzrouter/v1/access/console-grants", struct{}{}, &response, "authorize console entry"); err != nil {
		return nil, err
	}
	grant := response.Data
	parsed, err := url.Parse(grant.URL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/console/" {
		return nil, fmt.Errorf("coordinator returned an invalid console entry")
	}
	issuer := parsed.Scheme + "://" + parsed.Host
	if err := oauth.ValidateIssuer(issuer); err != nil {
		return nil, fmt.Errorf("coordinator returned an invalid console origin: %w", err)
	}
	if grant.Token == "" || len(grant.Token) > 256 || grant.ExpiresAt.IsZero() || grant.Purpose != "setup" && grant.Purpose != "session" {
		return nil, fmt.Errorf("coordinator returned an invalid console code")
	}
	grant.URL, err = consoleTargetURL(issuer, target)
	if err != nil {
		return nil, err
	}
	return &grant, nil
}
