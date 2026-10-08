package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// ============================================================================
// OpenAPI spec serving
// ============================================================================
//
// The hand-authored spec lives at internal/server/openapi.yaml and is embedded at build
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

// embeddedOpenAPI shares one immutable parse between JSON serving and operation search.
var embeddedOpenAPI = sync.OnceValues(func() (map[string]any, error) {
	var document map[string]any
	err := yaml.Unmarshal(openapiYAML, &document)
	return document, err
})
var openapiJSON = sync.OnceValues(func() ([]byte, error) {
	document, err := embeddedOpenAPI()
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
})

// registerOpenAPIRoutes registers /openapi.yaml and /openapi.json at the root.
// Called from setupRoutes on all node roles (workers included) so an agent
// hitting any endpoint can discover the schema.
func (s *Server) registerOpenAPIRoutes() {
	s.engine.GET("/openapi.yaml", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/yaml; charset=utf-8", openapiYAML)
	})
	s.engine.GET("/openapi.json", func(c *gin.Context) {
		data, err := openapiJSON()
		if err != nil {
			InternalNodeError(c, "failed to encode embedded OpenAPI JSON")
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	})
}

// searchOpenAPI projects the served document directly, without a second operation index.
func searchOpenAPI(query string, limit int) ([]map[string]any, error) {
	document, err := embeddedOpenAPI()
	if err != nil {
		return nil, fmt.Errorf("parse embedded OpenAPI: %w", err)
	}
	paths, _ := document["paths"].(map[string]any)
	results := []map[string]any{}
	for path, value := range paths {
		item, _ := value.(map[string]any)
		for method, value := range item {
			operation, _ := value.(map[string]any)
			id, _ := operation["operationId"].(string)
			if id == "" {
				continue
			}
			summary, _ := operation["summary"].(string)
			parameters := []string{}
			for _, value := range []any{item["parameters"], operation["parameters"]} {
				list, _ := value.([]any)
				for _, value := range list {
					parameter, _ := value.(map[string]any)
					if ref, ok := parameter["$ref"].(string); ok {
						parameter = document
						for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
							parameter, _ = parameter[segment].(map[string]any)
						}
					}
					if name, ok := parameter["name"].(string); ok {
						parameters = append(parameters, name)
					}
				}
			}
			text := strings.ToLower(method + " " + path + " " + id + " " + summary + " " + strings.Join(parameters, " "))
			matches := true
			for _, word := range strings.Fields(strings.ToLower(query)) {
				matches = matches && strings.Contains(text, word)
			}
			if matches {
				results = append(results, map[string]any{"method": strings.ToUpper(method), "path": path, "operationId": id, "summary": summary, "parameters": parameters})
			}
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return fmt.Sprint(results[i]["path"], results[i]["method"]) < fmt.Sprint(results[j]["path"], results[j]["method"])
	})
	return results[:min(limit, len(results))], nil
}
