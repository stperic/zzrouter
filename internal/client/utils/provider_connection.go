package client

import (
	"context"

	"github.com/stperic/zzrouter/pkg/apipath"
	"github.com/stperic/zzrouter/pkg/config"
)

// ProviderConnection contains safe state and the fields declared by the server.
type ProviderConnection struct {
	Provider         string                   `json:"provider"`
	Scope            string                   `json:"scope"`
	Mode             string                   `json:"mode"`
	Revision         uint64                   `json:"revision"`
	Configured       bool                     `json:"configured"`
	Enabled          bool                     `json:"enabled"`
	ManagedAvailable bool                     `json:"managed_available"`
	Fields           []config.ConnectionField `json:"fields"`
	Values           map[string]string        `json:"values"`
	Present          []string                 `json:"present"`
}

// ConnectionTestResult describes candidate validation without persisting values.
type ConnectionTestResult struct {
	Status     string `json:"status"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// ConnectionApplyResult separates persistence, durability, activation and publication.
type ConnectionApplyResult struct {
	Connection *ProviderConnection `json:"connection"`
	Saved      bool                `json:"saved"`
	Durable    bool                `json:"durable"`
	Active     bool                `json:"active"`
	Published  bool                `json:"published"`
	Outcome    string              `json:"outcome"`
}

// GetProviderConnection reads metadata without returning stored secrets.
func (c *Client) GetProviderConnection(ctx context.Context, name string) (*ProviderConnection, error) {
	var response struct {
		Data ProviderConnection `json:"data"`
	}
	if err := c.doContextJSON(ctx, "GET", apipath.ProviderConnection(name), nil, &response, "get provider connection"); err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// TestProviderConnection validates candidate values without saving them.
func (c *Client) TestProviderConnection(ctx context.Context, name string, values map[string]string) (*ConnectionTestResult, error) {
	request := struct {
		Values map[string]string `json:"values"`
	}{values}
	var response struct {
		Data ConnectionTestResult `json:"data"`
	}
	if err := c.doContextJSON(ctx, "POST", apipath.ProviderConnectionTest(name), request, &response, "test provider connection"); err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// SaveProviderConnection replaces the credential record at revision and optionally activates it.
func (c *Client) SaveProviderConnection(ctx context.Context, name string, revision uint64, values map[string]string, activate bool) (*ConnectionApplyResult, error) {
	request := struct {
		ExpectedRevision uint64            `json:"expected_revision"`
		Values           map[string]string `json:"values"`
		Activate         bool              `json:"activate"`
	}{revision, values, activate}
	var response struct {
		Data ConnectionApplyResult `json:"data"`
	}
	if err := c.doContextJSON(ctx, "PUT", apipath.ProviderConnection(name), request, &response, "save provider connection"); err != nil {
		return nil, err
	}
	return &response.Data, nil
}

// RemoveProviderConnection disables the connection and removes managed values at revision.
func (c *Client) RemoveProviderConnection(ctx context.Context, name string, revision uint64) (*ConnectionApplyResult, error) {
	return c.changeConnectionOwnership(ctx, "DELETE", apipath.ProviderConnection(name), revision)
}

// UseExternalProviderConnection switches to operator-provided configuration at revision.
func (c *Client) UseExternalProviderConnection(ctx context.Context, name string, revision uint64) (*ConnectionApplyResult, error) {
	return c.changeConnectionOwnership(ctx, "POST", apipath.ProviderConnectionUseExternal(name), revision)
}

func (c *Client) changeConnectionOwnership(ctx context.Context, method, path string, revision uint64) (*ConnectionApplyResult, error) {
	request := struct {
		ExpectedRevision uint64 `json:"expected_revision"`
	}{revision}
	var response struct {
		Data ConnectionApplyResult `json:"data"`
	}
	if err := c.doContextJSON(ctx, method, path, request, &response, "change connection ownership"); err != nil {
		return nil, err
	}
	return &response.Data, nil
}
