package client

import (
	"net/http"
	"net/url"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// ResolvedValue is one parameter or environment value as a launch of the
// requested model on the requested node would get it, and where it came
// from.
type ResolvedValue struct {
	Value any `json:"value"`
	// Tier is default, model, node, node-model or request.
	Tier  string `json:"tier"`
	Node  string `json:"node,omitempty"`
	Model string `json:"model,omitempty"`
	// Pattern is the model key that matched when it is not the model's
	// own name: a glob, or a shipped model default's family pattern.
	Pattern string `json:"pattern,omitempty"`
	// SHA256 is the content of the asset an asset-typed value names.
	SHA256 string `json:"sha256,omitempty"`
}

// ProviderResolved is GET /providers/:name/resolved.
type ProviderResolved struct {
	Parameters  map[string]ResolvedValue `json:"parameters"`
	Environment map[string]ResolvedValue `json:"environment"`
	// Request is the request-body defaults each request for the model
	// gets; a field the client sends wins.
	Request map[string]ResolvedValue `json:"request,omitempty"`
	// From is the base whose weights the model runs, when it is a variant.
	From string `json:"from,omitempty"`
}

// ProviderParametersWrite is a parameter write's answer: the resolved
// view it produced, and what it means for the models already running.
type ProviderParametersWrite struct {
	ProviderResolved
	Runs *RunsReport `json:"runs,omitempty"`
}

// ModelCellUpdate is a change to one models.<model> cell, such as a
// variant. Parameters are typed against the provider's schema before they
// are sent, the way the parameter editor's are.
type ModelCellUpdate struct {
	// From makes the cell a variant over that model's weights.
	From string
	// Parameters sets launch parameters; Unset deletes them.
	Parameters map[string]string
	Unset      []string
	// Request sets request-body defaults; UnsetRequest deletes them.
	Request      map[string]any
	UnsetRequest []string
}

// UpdateModelCell merge-patches one models.<model> cell and answers with
// that model resolved. With restart, the runs the write leaves stale are
// restarted.
func (c *Client) UpdateModelCell(provider, model string, u ModelCellUpdate, restart bool) (*ProviderParametersWrite, error) {
	cell := map[string]any{}
	if u.From != "" {
		cell["from"] = u.From
	}
	if len(u.Parameters)+len(u.Unset) > 0 {
		var params map[string]any
		if len(u.Parameters) > 0 {
			params = c.typedRequest(provider, &UpdateParametersRequest{Parameters: u.Parameters}).Parameters
		} else {
			params = map[string]any{}
		}
		for _, k := range u.Unset {
			params[k] = nil
		}
		cell["parameters"] = params
	}
	if len(u.Request)+len(u.UnsetRequest) > 0 {
		request := make(map[string]any, len(u.Request)+len(u.UnsetRequest))
		for k, v := range u.Request {
			request[k] = v
		}
		for _, k := range u.UnsetRequest {
			request[k] = nil
		}
		cell["request"] = request
	}
	return c.patchModelCell(provider, model, cell, restart)
}

// DeleteModelCell removes one models.<model> cell, such as a variant.
func (c *Client) DeleteModelCell(provider, model string) error {
	_, err := c.patchModelCell(provider, model, nil, false)
	return err
}

// patchModelCell merge-patches models.<model>; a nil cell is JSON null,
// which deletes it. The answer resolves that model.
func (c *Client) patchModelCell(provider, model string, cell map[string]any, restart bool) (*ProviderParametersWrite, error) {
	query := writeQuery(restart)
	query.Set("model", model)
	var out ProviderParametersWrite
	if err := c.sendMergePatch(provider, query, map[string]any{"models": map[string]any{model: cell}}, &out, "patch model parameters"); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProviderResolved resolves a provider's parameter tree for one model
// on one node; either may be empty.
func (c *Client) GetProviderResolved(provider, node, model string) (*ProviderResolved, error) {
	q := url.Values{}
	if node != "" {
		q.Set("node", node)
	}
	if model != "" {
		q.Set("model", model)
	}
	var out ProviderResolved
	if err := c.doJSON(http.MethodGet, withQuery(apipath.ProviderResolved(provider), q), nil, &out, "get resolved parameters"); err != nil {
		return nil, err
	}
	return &out, nil
}
