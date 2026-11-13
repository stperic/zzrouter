package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stperic/zzrouter/pkg/cluster/mesh"
	clusternode "github.com/stperic/zzrouter/pkg/cluster/node"
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

func TestRemoteLogsNeverForwardCallerAuthority(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		name := "native"
		if delegated {
			name = "oauth"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan *http.Request, 1)
			worker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Clone(r.Context())
				_, _ = w.Write([]byte(`{"lines":["fixture"]}`))
			}))
			defer worker.Close()
			cluster, err := mesh.NewCluster(&mesh.Config{NodeName: "coordinator"}, "http://coordinator.invalid", nil, nil, mesh.NewConnector(mesh.ConnectorConfig{HTTPClient: worker.Client()}))
			require.NoError(t, err)
			require.NoError(t, cluster.RegisterEndpoint(&mesh.Endpoint{NodeName: "worker", URL: worker.URL, ClusterURL: worker.URL, Status: mesh.StatusUp}))
			s := &Server{}
			s.cluster.coordinator = cluster
			handler := &LogsHandlers{routeToClusterNode: s.routeToClusterNode}
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			ctx := context.WithValue(t.Context(), oauthCredentialKey{}, delegated)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/zzrouter/v1/runs/example/logs?grep=fixture", nil)
			for _, header := range nodeAPIKeyHeaders {
				c.Request.Header.Set(header, "caller-authority")
			}
			c.Request.Header.Set("Cookie", browserSessionCookie+"=session; "+browserCSRFCookie+"=proof; "+browserHostSessionCookie+"=secure-session; "+browserHostCSRFCookie+"=secure-proof")
			handler.routeInstanceLogsToCluster(c, "worker", "example", 20, false)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			got := <-received
			for _, header := range nodeAPIKeyHeaders {
				require.Empty(t, got.Header.Values(header), header)
			}
			require.Empty(t, got.Header.Values("Cookie"))
			require.Equal(t, "true", got.Header.Get(clusternode.ClusterInternalHeader))
			require.Equal(t, "fixture", got.URL.Query().Get("grep"))
			require.Equal(t, "/zzrouter/v1/internal/runs/example/logs", got.URL.Path)
			require.NotEmpty(t, c.Request.Header.Get("Cookie"), "forwarding must not mutate the caller request")
		})
	}
}
