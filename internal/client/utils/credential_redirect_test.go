package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	pkgConfig "github.com/stperic/zzrouter/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestClientNeverReplaysCredentialsToAnotherOrigin(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var received atomic.Int32
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { received.Add(1) }))
		defer destination.Close()
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, status)
		}))
		defer origin.Close()
		client := NewClient(pkgConfig.ClientNodeConfig{Address: origin.URL, APIKey: "synthetic-coordinator-key"})
		candidate := map[string]string{"api_key": "synthetic-provider-key"}
		_, err := client.SaveProviderConnection(t.Context(), "openai", 0, candidate, false)
		require.Error(t, err, "a redirect is not a saved connection")
		calls := map[string]func() (*http.Response, error){
			"normal": func() (*http.Response, error) {
				return client.makeContextRequest(t.Context(), http.MethodPut, "/operation", candidate, nil)
			},
			"long": func() (*http.Response, error) {
				return client.makeLongRequest(http.MethodPut, "/operation", candidate)
			},
			"stream": func() (*http.Response, error) {
				return client.makeStreamingRequest(t.Context(), http.MethodPut, "/operation", candidate, nil)
			},
		}
		for name, call := range calls {
			t.Run(http.StatusText(status)+"/"+name, func(t *testing.T) {
				response, err := call()
				require.NoError(t, err)
				defer response.Body.Close()
				require.Equal(t, status, response.StatusCode)
				require.Zero(t, received.Load(), "neither router header nor provider body may leave the configured origin")
			})
		}
	}
}
