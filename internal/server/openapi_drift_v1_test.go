// OpenAPI drift check for the /v1/* (OpenAI-compatibility) surface.
//
// This test is the enforcement point for three invariants:
//
//  1. Every /v1/* route registered on the gin engine has a matching
//     path and method declared in internal/server/openapi.yaml.
//     Adding a new route without updating the spec breaks this test.
//
//  2. Every /v1/* path declared in the spec has a matching route
//     registered on the gin engine. Deleting a route without
//     removing its spec entry breaks this test.
//
//  3. The closed-vocabulary error.type and error.code enums declared
//     in the spec exactly match the ones defined in
//     pkg/protocol/openai/vocab.go. A Go constant added without a spec
//     enum update (or vice versa) breaks this test.
//
// The test is intentionally strict: a diff is a hard failure, not a
// warning. The goal is that a developer who adds /v1/foo can never
// ship without having to update the spec.
//
// The drift check used to exempt the 58 stateful /v1/* pass-through
// routes (/v1/files, /v1/batches, /v1/assistants, /v1/threads,
// /v1/vector_stores, /v1/fine_tuning, /v1/uploads, /v1/responses/{id})
// because they were not yet enumerated in openapi.yaml. That exemption
// has been lifted — the stateful block is now spec'd in full. If a
// future revision needs to exempt a temporarily-unspec'd path, add
// it to the (currently empty) exemption list below and document the
// follow-up ticket next to it.

package server

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	openaiproto "github.com/stperic/zzrouter/pkg/protocol/openai"
)

// driftExemptPrefixes names /v1/* path prefixes that are temporarily
// allowed to drift between gin registration and the spec. Adding an
// entry here is technical debt — the PR that adds it MUST link the
// follow-up ticket that removes it. The list is intentionally kept
// empty in the steady state; the drift check is strict.
var driftExemptPrefixes []string

func isDriftExempt(path string) bool {
	for _, p := range driftExemptPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// openAPIFile is a minimal yaml.v3 target that extracts just the
// fields the drift check needs: path → method → {} and components
// schemas OpenAIErrorType / OpenAIErrorCode enums.
type openAPIFile struct {
	Tags []struct {
		Name string `yaml:"name"`
	} `yaml:"tags"`
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas struct {
			OpenAIErrorType struct {
				Enum []string `yaml:"enum"`
			} `yaml:"OpenAIErrorType"`
			OpenAIErrorCode struct {
				Enum []string `yaml:"enum"`
			} `yaml:"OpenAIErrorCode"`
			ParamError struct {
				Properties struct {
					Code struct {
						Enum []string `yaml:"enum"`
					} `yaml:"code"`
				} `yaml:"properties"`
			} `yaml:"ParamError"`
			ResolvedValue struct {
				Properties struct {
					Tier struct {
						Enum []string `yaml:"enum"`
					} `yaml:"tier"`
				} `yaml:"properties"`
			} `yaml:"ResolvedValue"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

// loadOpenAPI parses openapi.yaml from the repo root. The path is
// resolved relative to this test file so `go test` works from any
// working directory.
func loadOpenAPI(t *testing.T) *openAPIFile {
	t.Helper()
	// The test binary runs in the package directory, so the spec
	// is right next to us.
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		// Fallback in case the test is invoked from the repo root.
		alt := filepath.Join("internal", "server", "openapi.yaml")
		data, err = os.ReadFile(alt)
		if err != nil {
			t.Fatalf("read openapi.yaml: %v", err)
		}
	}
	var spec openAPIFile
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	return &spec
}

// routeKey is a "METHOD path" pair used as a map key. Methods are
// uppercased for canonical comparison.
type routeKey string

func rk(method, path string) routeKey {
	return routeKey(strings.ToUpper(method) + " " + path)
}

// collectSpecV1Routes walks the parsed spec and returns the set of
// (method, path) pairs under /v1/*. Paths outside /v1/* are ignored.
func collectSpecV1Routes(spec *openAPIFile) map[routeKey]struct{} {
	out := map[routeKey]struct{}{}
	for path, ops := range spec.Paths {
		if !strings.HasPrefix(path, "/v1") {
			continue
		}
		for method := range ops {
			// Skip the shared `parameters` key which sits at the
			// operation level alongside get/post/... in OpenAPI.
			if method == "parameters" {
				continue
			}
			out[rk(method, path)] = struct{}{}
		}
	}
	return out
}

// collectGinV1Routes pulls the set of (method, path) pairs from the
// given gin engine, filtered to /v1/* and normalized so gin's wildcard
// syntax matches OpenAPI's `{param}` placeholder syntax.
//
// Normalization rules:
//
//   - "/v1/models/*model"          → "/v1/models/{model}"
//   - "/v1/foo/:bar"               → "/v1/foo/{bar}"
//   - Gin's default 404/405 pseudo-routes are dropped.
func collectGinV1Routes(routes []ginRouteInfo) map[routeKey]struct{} {
	out := map[routeKey]struct{}{}
	for _, r := range routes {
		if !strings.HasPrefix(r.Path, "/v1") {
			continue
		}
		if isDriftExempt(r.Path) {
			continue
		}
		path := normalizeGinPath(r.Path)
		out[rk(r.Method, path)] = struct{}{}
	}
	return out
}

// ginRouteInfo is the minimal subset of gin.RouteInfo the test cares
// about. Kept as a local alias so the test file does not couple to
// the full gin import surface.
type ginRouteInfo struct {
	Method string
	Path   string
}

// normalizeGinPath rewrites gin's route patterns into OpenAPI's
// {param} placeholder syntax.
func normalizeGinPath(path string) string {
	// Fast path: nothing special to rewrite.
	if !strings.ContainsAny(path, ":*") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		switch {
		case strings.HasPrefix(seg, "*"):
			segments[i] = "{" + seg[1:] + "}"
		case strings.HasPrefix(seg, ":"):
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// TestOpenAPIDrift_V1_RoutesMatchSpec is the main drift assertion.
// It spins up a test server to walk its registered routes, parses
// the embedded openapi.yaml, and diffs the two sets.
func TestOpenAPIDrift_V1_RoutesMatchSpec(t *testing.T) {
	// createTestNode registers its own cleanup — no extra hook needed.
	s := createTestNode(t, DefaultTestNodeConfig())

	// Collect the live gin routes.
	engineRoutes := s.engine.Routes()
	ginRoutes := make([]ginRouteInfo, 0, len(engineRoutes))
	for _, r := range engineRoutes {
		ginRoutes = append(ginRoutes, ginRouteInfo{Method: r.Method, Path: r.Path})
	}

	spec := loadOpenAPI(t)
	specSet := collectSpecV1Routes(spec)
	ginSet := collectGinV1Routes(ginRoutes)

	// Routes registered on the engine but missing from the spec.
	var missingFromSpec []string
	for k := range ginSet {
		if _, ok := specSet[k]; !ok {
			missingFromSpec = append(missingFromSpec, string(k))
		}
	}
	sort.Strings(missingFromSpec)

	// Routes declared in the spec but not registered on the engine.
	var extraInSpec []string
	for k := range specSet {
		if _, ok := ginSet[k]; !ok {
			extraInSpec = append(extraInSpec, string(k))
		}
	}
	sort.Strings(extraInSpec)

	if len(missingFromSpec) > 0 {
		t.Errorf("the following /v1/* routes are registered on the engine but missing from openapi.yaml:\n  %s\n\nEither add them to the spec or add their prefix to statefulPassThroughPrefixes.",
			strings.Join(missingFromSpec, "\n  "))
	}
	if len(extraInSpec) > 0 {
		t.Errorf("the following /v1/* routes are declared in openapi.yaml but not registered on the engine:\n  %s\n\nEither register them or remove from the spec.",
			strings.Join(extraInSpec, "\n  "))
	}
}

// TestOpenAPIDrift_V1_ErrorTypeEnumMatchesVocab ensures the spec's
// OpenAIErrorType enum is exactly the set of values declared by
// pkg/protocol/openai.AllErrorTypes.
func TestOpenAPIDrift_V1_ErrorTypeEnumMatchesVocab(t *testing.T) {
	spec := loadOpenAPI(t)
	specEnum := sortedStrings(spec.Components.Schemas.OpenAIErrorType.Enum)
	vocab := make([]string, 0, len(openaiproto.AllErrorTypes()))
	for _, v := range openaiproto.AllErrorTypes() {
		vocab = append(vocab, string(v))
	}
	vocab = sortedStrings(vocab)
	if !equalStringSlices(specEnum, vocab) {
		t.Errorf("OpenAIErrorType drift:\n  spec:  %v\n  vocab: %v", specEnum, vocab)
	}
}

// TestOpenAPIDrift_V1_ErrorCodeEnumMatchesVocab mirrors the above for
// error.code.
func TestOpenAPIDrift_V1_ErrorCodeEnumMatchesVocab(t *testing.T) {
	spec := loadOpenAPI(t)
	specEnum := sortedStrings(spec.Components.Schemas.OpenAIErrorCode.Enum)
	vocab := make([]string, 0, len(openaiproto.AllErrorCodes()))
	for _, v := range openaiproto.AllErrorCodes() {
		vocab = append(vocab, string(v))
	}
	vocab = sortedStrings(vocab)
	if !equalStringSlices(specEnum, vocab) {
		t.Errorf("OpenAIErrorCode drift:\n  spec:  %v\n  vocab: %v", specEnum, vocab)
	}
}

func sortedStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
