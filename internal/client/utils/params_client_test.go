package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParameterWritesUseScopedMergePatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scope    ParameterScope
		envelope string
	}{
		{"defaults", ParameterScope{Scope: "defaults"}, `{"defaults":{"parameters":{"threads":8,"removed":null},"environment":{"EMPTY":""}}}`},
		{"model", ParameterScope{Scope: "model", Model: "q/a"}, `{"models":{"q/a":{"parameters":{"threads":8,"removed":null},"environment":{"EMPTY":""}}}}`},
		{"node", ParameterScope{Scope: "node", Node: "worker"}, `{"nodes":{"worker":{"parameters":{"threads":8,"removed":null},"environment":{"EMPTY":""}}}}`},
		{"node model", ParameterScope{Scope: "node-model", Node: "worker", Model: "q/a"}, `{"nodes":{"worker":{"models":{"q/a":{"parameters":{"threads":8,"removed":null},"environment":{"EMPTY":""}}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPatch, r.Method)
				assert.Equal(t, "/zzrouter/v1/providers/test/parameters", r.URL.Path)
				assert.Equal(t, mergePatchContentType, r.Header.Get("Content-Type"))
				require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
				_, _ = io.WriteString(w, `{}`)
			}))
			value, empty := "8", ""
			err := client.PatchConfiguredParameters(context.Background(), "test", tc.scope, ParameterChanges{
				Parameters: map[string]*string{"threads": &value, "removed": nil}, Environment: map[string]*string{"EMPTY": &empty},
			}, &ProviderSchemaResponse{Parameters: map[string]ParameterSchema{"threads": {Type: "int"}}})
			require.NoError(t, err)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			assert.JSONEq(t, tc.envelope, string(encoded))
		})
	}
}

func TestConfiguredParameterReadPreservesPresence(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "model", r.URL.Query().Get("scope"))
		assert.Equal(t, "q/a", r.URL.Query().Get("model"))
		_, _ = io.WriteString(w, `{"scope":"model","model":"q/a","parameters":{"empty":""},"environment":{}}`)
	}))
	got, err := client.GetConfiguredParameters(context.Background(), "test", ParameterScope{Scope: "model", Model: "q/a"})
	require.NoError(t, err)
	assert.Contains(t, got.Parameters, "empty")
	assert.NotContains(t, got.Parameters, "inherited")
}

// Values are typed against the schema before they go on the wire.
//
// Everything the editor collects is a string, but Merge-Patch rejects
// "6" for an int key with wrong_type, which is how every save of a
// numeric parameter would have failed even after the routes were fixed.
// Found by driving the real coordinator, not the mock.
func TestTypeValues_CoercesAgainstSchema(t *testing.T) {
	schema := map[string]ParameterSchema{
		"threads":       {Type: "int"},
		"context-shift": {Type: "bool"},
		"alias":         {Type: "string"},
		"temp":          {Type: "float"},
	}
	got := typeValues(schema, map[string]string{
		"threads":       "6",
		"context-shift": "true",
		"alias":         "my-model",
		"temp":          "0.7",
		"unknown":       "42",     // not in schema: pass through untouched
		"bad-int":       "twelve", // unparseable: let the server say so
	})
	want := map[string]any{
		"threads":       int64(6),
		"context-shift": true,
		"alias":         "my-model",
		"temp":          0.7,
		"unknown":       "42",
		"bad-int":       "twelve",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
	// A string-typed key holding digits must stay a string, or a schema
	// expecting text would start receiving numbers.
	if _, isStr := typeValues(schema, map[string]string{"alias": "8192"})["alias"].(string); !isStr {
		t.Error("a string-typed key was coerced to a number")
	}
}
