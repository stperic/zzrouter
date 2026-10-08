// OpenAPI drift check for the /api/* (Ollama-compatibility) surface.
//
// Mirrors openapi_drift_v1_test.go but for the Ollama dialect. The
// two invariants enforced here are:
//
//  1. Every /api/* route registered on the gin engine has a matching
//     path and method declared in internal/server/openapi.yaml.
//  2. Every /api/* path declared in the spec has a matching route
//     registered on the gin engine.
//
// There is NO error-vocabulary check for the Ollama surface — Ollama's
// error envelope is the free-form flat shape {"error": "<string>"},
// not a closed vocabulary. If zzRouter ever introduces a closed
// vocabulary for /api/*, add an enum-drift assertion here mirroring
// the OpenAI one.
//
// Shared helpers (loadOpenAPI, normalizeGinPath, rk, routeKey,
// equalStringSlices, sortedStrings) come from openapi_drift_v1_test.go
// and are reused as-is — both tests are in the same package so the
// unexported helpers are visible to both.

package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// apiDriftExemptPrefixes names /api/* path prefixes temporarily
// allowed to drift between gin and the spec. Always empty in the
// steady state — the drift check is strict. Same convention as
// driftExemptPrefixes for /v1/*.
var apiDriftExemptPrefixes []string

func isAPIDriftExempt(path string) bool {
	for _, p := range apiDriftExemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// collectSpecAPIRoutes walks the parsed spec and returns the set of
// (method, path) pairs under /api/*. Paths outside /api/* are ignored.
// Mirrors collectSpecV1Routes — kept as a separate function so the
// prefix filter is local and the intent is obvious at the call site.
func collectSpecAPIRoutes(spec *openAPIFile) map[routeKey]struct{} {
	out := map[routeKey]struct{}{}
	for path, ops := range spec.Paths {
		if !strings.HasPrefix(path, "/api/") {
			continue
		}
		for method := range ops {
			if method == "parameters" {
				continue
			}
			out[rk(method, path)] = struct{}{}
		}
	}
	return out
}

// collectGinAPIRoutes pulls the set of (method, path) pairs from the
// given gin route list, filtered to /api/* and normalized so gin's
// wildcard syntax matches OpenAPI's `{param}` placeholders.
func collectGinAPIRoutes(routes []ginRouteInfo) map[routeKey]struct{} {
	out := map[routeKey]struct{}{}
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "/api/") {
			continue
		}
		if isAPIDriftExempt(r.Path) {
			continue
		}
		path := normalizeGinPath(r.Path)
		out[rk(r.Method, path)] = struct{}{}
	}
	return out
}

// TestOpenAPIDrift_API_RoutesMatchSpec is the Ollama mirror of
// TestOpenAPIDrift_V1_RoutesMatchSpec. It spins up a test server and
// diffs the live route registration against openapi.yaml's /api/*
// paths. A diff in either direction fails the test — the drift
// check is intentionally strict.
func TestOpenAPIDrift_API_RoutesMatchSpec(t *testing.T) {
	// createTestNode registers its own cleanup — no extra hook needed.
	s := createTestNode(t, DefaultTestNodeConfig())

	engineRoutes := s.engine.Routes()
	ginRoutes := make([]ginRouteInfo, 0, len(engineRoutes))
	for _, r := range engineRoutes {
		ginRoutes = append(ginRoutes, ginRouteInfo{Method: r.Method, Path: r.Path})
	}

	spec := loadOpenAPI(t)
	specSet := collectSpecAPIRoutes(spec)
	ginSet := collectGinAPIRoutes(ginRoutes)

	var missingFromSpec []string
	for k := range ginSet {
		if _, ok := specSet[k]; !ok {
			missingFromSpec = append(missingFromSpec, string(k))
		}
	}
	sort.Strings(missingFromSpec)

	var extraInSpec []string
	for k := range specSet {
		if _, ok := ginSet[k]; !ok {
			extraInSpec = append(extraInSpec, string(k))
		}
	}
	sort.Strings(extraInSpec)

	if len(missingFromSpec) > 0 {
		t.Errorf("the following /api/* routes are registered on the engine but missing from openapi.yaml:\n  %s\n\nEither add them to the spec or add their prefix to apiDriftExemptPrefixes.",
			strings.Join(missingFromSpec, "\n  "))
	}
	if len(extraInSpec) > 0 {
		t.Errorf("the following /api/* routes are declared in openapi.yaml but not registered on the engine:\n  %s\n\nEither register them or remove from the spec.",
			strings.Join(extraInSpec, "\n  "))
	}
}

// TestOllamaErrorSchema_Declared is a structural assertion: the
// Ollama error envelope schema MUST exist in the spec with `error`
// as a required field. If a future cleanup pass removes the schema
// by accident, this test fires before any /api/* operation that
// references it via $ref silently breaks.
func TestOllamaErrorSchema_Declared(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		data, err = os.ReadFile(filepath.Join("internal", "server", "openapi.yaml"))
		if err != nil {
			t.Fatalf("read openapi.yaml: %v", err)
		}
	}

	var spec struct {
		Components struct {
			Schemas struct {
				OllamaError struct {
					Required []string `yaml:"required"`
				} `yaml:"OllamaError"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	found := false
	for _, r := range spec.Components.Schemas.OllamaError.Required {
		if r == "error" {
			found = true
			break
		}
	}
	if !found {
		t.Error("components.schemas.OllamaError must declare `error` as a required field")
	}
}
