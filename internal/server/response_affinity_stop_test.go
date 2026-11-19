package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/prov_apps/instance"
	"github.com/stretchr/testify/require"
)

func TestResponseAffinity_RunStopCancelsBeforePortReuse(t *testing.T) {
	for _, operation := range append(responseResourceRequests, struct{ method, suffix string }{http.MethodPost, "continue"}) {
		t.Run(operation.method+operation.suffix, func(t *testing.T) {
			old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_stop_race","object":"response"}`))
			}))
			t.Cleanup(old.Close)
			u, err := url.Parse(old.URL)
			require.NoError(t, err)
			port, err := strconv.Atoi(u.Port())
			require.NoError(t, err)
			s, run := responseRunNode(t, port, "local", config.ClusterModeDisabled)
			created := postAsAdmin(t, s, "/v1/responses", `{"model":"qwen","input":"hi"}`)
			require.Equal(t, http.StatusOK, created.Code, created.Body.String())
			entered, release := make(chan struct{}), make(chan struct{})
			releaseGate := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseGate)
			base := s.proxy.streamClient.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			s.proxy.streamClient = &http.Client{Transport: mcpRoundTripper(func(r *http.Request) (*http.Response, error) {
				close(entered)
				select {
				case <-r.Context().Done():
					return nil, r.Context().Err()
				case <-release:
				}
				return base.RoundTrip(r)
			})}
			result := make(chan *TestResponse, 1)
			go func() {
				path := "/v1/responses/resp_stop_race" + operation.suffix
				var body any
				if operation.suffix == "continue" {
					path = "/v1/responses"
					body = map[string]any{"model": "qwen", "input": "again", "previous_response_id": "resp_stop_race"}
				}
				result <- makeAuthRequest(t, s, operation.method, path, TestAdminKey, body)
			}()
			<-entered
			require.NoError(t, s.providers.appMgr.StopInstance(t.Context(), run.ID))
			old.Close()
			var replacementCalls atomic.Int32
			listener, err := net.Listen("tcp", u.Host)
			require.NoError(t, err)
			replacement := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				replacementCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"replacement"}`))
			}))
			replacement.Listener = listener
			replacement.Start()
			t.Cleanup(replacement.Close)
			next := instance.NewInstance("replacement", "llamacpp", "qwen", port, 0, 0)
			next.MarkRunning(false, nil)
			require.NoError(t, s.providers.appMgr.Instances().Register(next))
			t.Cleanup(func() { _ = s.providers.appMgr.Instances().Remove(next.ID) })
			releaseGate()
			response := <-result
			require.Zero(t, replacementCalls.Load(), "a leased request reached a replacement process: status %d body %s", response.Code, string(response.Body))
			require.GreaterOrEqual(t, response.Code, http.StatusBadRequest, string(response.Body))
		})
	}
}
