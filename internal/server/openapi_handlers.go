package server

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// ============================================================================
// OpenAPI spec serving
// ============================================================================
//
// The hand-authored spec lives at docs/openapi.yaml and is embedded at build
// time. We expose it on two no-auth routes:
//
//   GET /openapi.yaml  — raw YAML (application/yaml)
//   GET /openapi.json  — JSON conversion (application/json)
//
// These endpoints are intentionally unauthenticated so AI agents can fetch
// the schema before they have credentials. The spec only describes paths
// and shapes — it does not leak runtime configuration.

//go:embed openapi.yaml
var openapiYAML []byte

// openapiJSONCache holds the lazily-computed JSON conversion of the YAML spec.
// Built on first request and reused; the source bytes never change at runtime.
var openapiJSONCache []byte

// registerOpenAPIRoutes registers /openapi.yaml and /openapi.json at the root.
// Called from setupRoutes on all node roles (workers included) so an agent
// hitting any endpoint can discover the schema.
func (s *Server) registerOpenAPIRoutes() {
	s.engine.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", openapiYAML)
	})
	s.engine.GET("/openapi.json", func(c *gin.Context) {
		if openapiJSONCache == nil {
			var raw any
			if err := yaml.Unmarshal(openapiYAML, &raw); err != nil {
				InternalNodeError(c, "failed to parse embedded OpenAPI YAML")
				return
			}
			normalized := normalizeYAMLForJSON(raw)
			out, err := json.Marshal(normalized)
			if err != nil {
				InternalNodeError(c, "failed to encode OpenAPI JSON")
				return
			}
			openapiJSONCache = out
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", openapiJSONCache)
	})
}

// normalizeYAMLForJSON walks the decoded YAML tree and rewrites any
// map[interface{}]interface{} into map[string]interface{} so the result is
// JSON-marshalable. The go-yaml v3 decoder already produces string-keyed maps
// for top-level structs, but nested anonymous maps can still come back with
// interface keys depending on the source.
func normalizeYAMLForJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = normalizeYAMLForJSON(val)
		}
		return m
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			if ks, ok := k.(string); ok {
				m[ks] = normalizeYAMLForJSON(val)
			}
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeYAMLForJSON(item)
		}
		return out
	default:
		return v
	}
}
