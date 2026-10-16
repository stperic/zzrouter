package ollama

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/constants"
	"github.com/stperic/zzrouter/pkg/download"
	"github.com/stretchr/testify/require"
)

type pullTransport func(*http.Request) (*http.Response, error)

func (f pullTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPullModelBodyStall(t *testing.T) {
	for _, trickle := range []bool{false, true} {
		name := "stalled"
		if trickle {
			name = "trickling"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body, writer := io.Pipe()
				defer func() { _ = body.Close(); _ = writer.Close() }()
				cfg := &config.AppsConfig{}
				provider := config.NewOllamaConnectProvider(constants.AppOllama, "http://fixture.test", "")
				require.NoError(t, cfg.AddApp(constants.AppOllama, config.ServiceConfig{
					Name: constants.AppOllama, Mode: constants.AppModeExternal, Enabled: new(true), Protocol: "ollama",
					Runtime: &config.AppRuntimeConfig{Endpoint: "http://fixture.test"}, Capabilities: provider.Capabilities,
				}))
				c := NewConnector()
				c.ReloadConfig(cfg)
				c.pullClient.Transport = pullTransport(func(r *http.Request) (*http.Response, error) {
					require.Equal(t, "/api/pull", r.URL.Path)
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				})
				done := make(chan error, 1)
				go func() { done <- c.PullModelWithContext(t.Context(), "model", nil) }()
				_, err := io.WriteString(writer, `{"status":`)
				require.NoError(t, err)
				synctest.Wait()
				if trickle {
					// The first decoded frame takes over ten minutes; raw bytes keep it alive.
					for _, b := range []byte(`"success"}`) {
						time.Sleep(download.BodyStallTimeout / 2)
						_, err := writer.Write([]byte{b})
						require.NoError(t, err)
						synctest.Wait()
					}
					require.NoError(t, writer.Close())
					require.NoError(t, <-done)
				} else {
					time.Sleep(download.BodyStallTimeout)
					synctest.Wait()
					select {
					case err := <-done:
						require.ErrorContains(t, err, "no data received for 10m0s")
						require.ErrorIs(t, err, context.DeadlineExceeded)
					default:
						t.Fatal("stalled Ollama body did not fail at the byte-silence deadline")
					}
				}
			})
		})
	}
}
