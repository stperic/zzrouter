package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The spec publishes `endpoint` as a closed set on both launch surfaces
// and only one of them enforced it: /runs carried the oneof tag from the
// start, /runs/load did not. An unknown value there reached the launch,
// which keys an instance by whatever string it is given -- so the caller
// got a 200 for a process no route would ever reach.
//
// Both bodies are checked in one table so the set cannot be widened on
// one surface and left behind on the other.
func TestLaunchSurfaces_AcceptOnlyTheEndpointsTheyServe(t *testing.T) {
	RegisterValidatorTranslator()
	t.Parallel()

	// bindStrict consumes the request body, so every case builds its own
	// body and its own destination.
	surfaces := []struct {
		name  string
		route string
		body  func(endpoint string) string
		req   func() any
	}{
		{
			name:  "runs",
			route: "POST /runs",
			body: func(e string) string {
				return `{"provider":"llamacpp","launch_mode":"native","model_name":"m","endpoint":"` + e + `"}`
			},
			req: func() any { return &LaunchRunRequest{} },
		},
		{
			name:  "runs_load",
			route: "POST /runs/load",
			body: func(e string) string {
				return `{"model_name":"m","endpoint":"` + e + `"}`
			},
			req: func() any { return &LoadModelRequest{} },
		},
	}

	// The values prov_apps.Endpoint declares.
	served := []string{"chat", "embeddings", "reranking"}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			for _, endpoint := range served {
				c, _ := postJSON(t, s.body(endpoint))
				assert.True(t, BindJSONStrict(c, s.req()), "%s rejected endpoint %q", s.route, endpoint)
			}

			c, rec := postJSON(t, s.body("embed"))
			require.False(t, BindJSONStrict(c, s.req()),
				"%s accepted an endpoint it cannot serve", s.route)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "endpoint",
				"the rejection should name the field the caller got wrong")
		})
	}
}

// Empty is the documented default, so the enum must not catch it: most
// callers never send the field at all.
func TestLaunchSurfaces_AnAbsentEndpointIsStillTheDefault(t *testing.T) {
	RegisterValidatorTranslator()
	t.Parallel()

	c, _ := postJSON(t, `{"model_name":"m"}`)
	assert.True(t, BindJSONStrict(c, &LoadModelRequest{}),
		"an omitted endpoint should bind as the chat default")
}
