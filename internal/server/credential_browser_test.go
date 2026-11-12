package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

func TestBrowserCookiesNeverForward(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		ctx := context.WithValue(t.Context(), oauthCredentialKey{}, delegated)
		for _, up := range []backend.Upstream{backend.Engine(), backend.CallerKey(), backend.Credential(&config.APIConfig{AuthType: config.AuthTypeAPIKey, AuthHeader: "X-API-Key", Token: "provider-key"}), backend.Cluster()} {
			header := http.Header{"Cookie": {browserSessionCookie + "=session-secret; " + browserCSRFCookie + "=csrf-proof; __Host-" + browserSessionCookie + "=secure-session; __Host-" + browserCSRFCookie + "=secure-proof; preference=dark"}, "X-Api-Key": {"native-caller-key"}}
			got := upstreamHeaders(ctx, header, up)
			require.Equal(t, "preference=dark", got.Get("Cookie"))
			require.Contains(t, header.Get("Cookie"), "session-secret", "source request is not mutated")
		}
	}
}
