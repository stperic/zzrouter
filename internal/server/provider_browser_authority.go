package server

import (
	"context"

	"github.com/stperic/common/mcp/oauth"
	"github.com/stperic/zzrouter/pkg/access/control"
)

// The gateway delegates durable client ownership to the canonical authority and
// resolves returned references through the same live key/team policy as MCP.
type providerBrowserOAuthAuthority struct {
	authority *oauth.Server[control.CredentialReference]
	core      *control.Core
}

func (a *providerBrowserOAuthAuthority) CreateClient(ctx context.Context) (string, error) {
	return a.authority.CreateOwnedClient(ctx, providerBrowserOwner)
}

func (a *providerBrowserOAuthAuthority) RetireClient(ctx context.Context, id string) error {
	return a.authority.RetireOwnedClient(ctx, providerBrowserOwner, id)
}

func (a *providerBrowserOAuthAuthority) ResetClients(ctx context.Context) error {
	return a.authority.ResetOwnedClients(ctx, providerBrowserOwner)
}

func (a *providerBrowserOAuthAuthority) Authenticate(ctx context.Context, token string) (*AccessContext, error) {
	ref, err := a.authority.Authenticate(ctx, token, a.authority.Issuer()+providerBrowserResource)
	if err != nil {
		return nil, err
	}
	return a.core.AuthenticateReference(ref)
}
