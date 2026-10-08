package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stperic/zzrouter/pkg/config"
)

// A cloud search resolves its provider when it runs, so an edit to the
// endpoint or the credential after registration is what the next search
// sends. It searches a provider not yet enabled, as onboarding does.
func TestCloudSearch_FollowsAnEditedProvider(t *testing.T) {
	serve := func(got *string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*got = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		}))
	}
	var gotOld, gotNew string
	oldSrv, newSrv := serve(&gotOld), serve(&gotNew)
	defer oldSrv.Close()
	defer newSrv.Close()

	cfg := &config.AppsConfig{}
	require.NoError(t, cfg.AddApp("cloudy", config.ServiceConfig{
		Enabled: new(false), Name: "Cloudy", Protocol: config.ProtocolOpenAI, Mode: "cloud",
		Runtime: &config.AppRuntimeConfig{
			Endpoint: oldSrv.URL,
			API:      &config.APIConfig{AuthType: config.AuthTypeBearer, Token: "old-token"},
		},
		Capabilities: &config.AppCapabilities{WireEndpoints: []string{"chat_completions"}},
	}))
	search := newCloudModelSearchFunc("cloudy", func() *config.AppsConfig { return cfg })

	_, err := search(context.Background(), "", 0)
	require.NoError(t, err)
	assert.Equal(t, "Bearer old-token", gotOld)

	require.NoError(t, cfg.UpdateApp("cloudy", func(sc *config.ServiceConfig) error {
		sc.Runtime.Endpoint = newSrv.URL
		sc.Runtime.API.Token = "new-token"
		return nil
	}))
	_, err = search(context.Background(), "", 0)
	require.NoError(t, err)
	assert.Equal(t, "Bearer new-token", gotNew)
}
