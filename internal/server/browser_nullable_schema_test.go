package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/stperic/zzrouter/pkg/config"
	"github.com/stperic/zzrouter/pkg/config/backend"
	"github.com/stretchr/testify/require"
)

// OpenAPI 3.0 nullable extends only an explicitly declared type; composition still applies.
func browserSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	doc := loadSpecTree(t)
	var convert func(any)
	convert = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if node["nullable"] == true {
				if kind, ok := node["type"].(string); ok {
					node["type"] = []any{kind, "null"}
				}
			}
			for _, child := range node {
				convert(child)
			}
		case []any:
			for _, child := range node {
				convert(child)
			}
		}
	}
	convert(doc)
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft4
	const location = "https://schema.example/openapi.json"
	require.NoError(t, compiler.AddResource(location, bytes.NewReader(data)))
	schema, err := compiler.Compile(location + "#/components/schemas/" + name)
	require.NoError(t, err)
	return schema
}

func TestConnectionCommittedWithoutStatusMatchesSchema(t *testing.T) {
	h := newConnectionHarness(t, "https://fixture.example")
	h.service.changed = func(string) {
		h.service.resolver = backend.NewResolver(func() *config.AppsConfig { return nil }, h.vault)
	}
	response := httptest.NewRecorder()
	body := `{"expected_revision":0,"values":{"endpoint":"https://fixture.example","token":"candidate-schema-fixture"}}`
	h.engine.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/zzrouter/v1/providers/fixture/connection", bytes.NewBufferString(body)))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	require.Equal(t, true, envelope.Data["saved"])
	require.Contains(t, envelope.Data, "connection")
	require.Nil(t, envelope.Data["connection"])
	schema := browserSchema(t, "ProviderConnectionApply")
	require.NoError(t, schema.Validate(envelope.Data))
	envelope.Data["connection"] = map[string]any{}
	require.Error(t, schema.Validate(envelope.Data), "nullable must not permit an incomplete status object")
	envelope.Data["connection"] = "unavailable"
	require.Error(t, schema.Validate(envelope.Data), "nullable must not permit a string status")
}

func TestBrowserIdentityAbsentOptionalObjectsMatchSchema(t *testing.T) {
	data, err := json.Marshal(browserIdentity{ID: "admin", Name: "Admin", Role: RoleAdmin})
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	require.Nil(t, value["team"])
	require.Nil(t, value["usage"])
	schema := browserSchema(t, "BrowserIdentity")
	require.NoError(t, schema.Validate(value))
	value["team"] = map[string]any{}
	require.Error(t, schema.Validate(value), "nullable must preserve the referenced team contract")
}
