// OpenAPI drift check for the /zzrouter/v1/* (management) surface.
//
// This is the surface an AI agent drives, and it is the one that rots
// quietest: a new endpoint works perfectly while being invisible to
// anyone reading the spec. The /v1/* and /api/* checks already exist;
// this closes the third.
//
// Only public routes are checked. /zzrouter/v1/internal/* is
// cluster-to-cluster plumbing gated on the cluster key — no client is
// meant to drive it, so documenting it would advertise a surface that
// is not a contract.
//
// Shared helpers (loadOpenAPI, normalizeGinPath, rk, routeKey) come
// from openapi_drift_v1_test.go — same package, so they are reused
// as-is.

package server

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// adminSpecGap lists public management routes that are registered but not
// yet described in openapi.yaml.
//
// It is empty, and that is the point: the backlog it ratcheted down is
// gone, so every registered management route is now documented and a new
// one fails this test until it is written up. Keep it empty. An entry
// added here buys a green build by making the spec less true, which is
// the failure this whole check exists to prevent.
var adminSpecGap = map[routeKey]struct{}{}

// adminSpecStale lists paths the spec declares that the engine does not
// register. Same rule in the other direction, and also empty: a stale
// entry hands a client a method that 404s, which is worse than an
// undocumented route because the client has no reason to doubt it.
var adminSpecStale = map[routeKey]struct{}{}

const adminPrefix = "/zzrouter/v1"
const adminInternalPrefix = "/zzrouter/v1/internal"

func isPublicAdminPath(path string) bool {
	return strings.HasPrefix(path, adminPrefix) && !strings.HasPrefix(path, adminInternalPrefix)
}

func TestOpenAPIDrift_Admin_RoutesMatchSpec(t *testing.T) {
	// Seeded providers/ so the routes gated on a config store register.
	// Without it POST /providers/instances is absent from the engine and
	// the spec entry for it reads as stale, which is a fact about the
	// test node rather than about the API.
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})

	spec := loadOpenAPI(t)
	specSet := map[routeKey]struct{}{}
	for path, ops := range spec.Paths {
		if !isPublicAdminPath(path) {
			continue
		}
		for method := range ops {
			if method == "parameters" {
				continue
			}
			specSet[rk(method, path)] = struct{}{}
		}
	}

	ginSet := map[routeKey]struct{}{}
	for _, r := range s.engine.Routes() {
		if !isPublicAdminPath(r.Path) {
			continue
		}
		ginSet[rk(r.Method, normalizeGinPath(r.Path))] = struct{}{}
	}

	var missing, stale, healed []string
	for k := range ginSet {
		if _, ok := specSet[k]; ok {
			if _, waived := adminSpecGap[k]; waived {
				healed = append(healed, string(k))
			}
			continue
		}
		if _, waived := adminSpecGap[k]; !waived {
			missing = append(missing, string(k))
		}
	}
	for k := range specSet {
		if _, ok := ginSet[k]; ok {
			continue
		}
		if _, waived := adminSpecStale[k]; !waived {
			stale = append(stale, string(k))
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	sort.Strings(healed)

	if len(missing) > 0 {
		t.Errorf("these management routes are registered but absent from openapi.yaml:\n  %s\n\n"+
			"An agent reading the spec cannot discover them. Describe them in internal/server/openapi.yaml.",
			strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("these management routes are declared in openapi.yaml but not registered:\n  %s\n\n"+
			"A client following the spec would get a 404. Register them or drop them from the spec.",
			strings.Join(stale, "\n  "))
	}
	// The ratchet only tightens: once documented, an entry must leave the
	// waiver list, or the list stops describing anything real.
	if len(healed) > 0 {
		t.Errorf("these routes are now documented and must be removed from adminSpecGap:\n  %s",
			strings.Join(healed, "\n  "))
	}
}

// TestOpenAPIDrift_Admin_RetiredRoutesAreMarkedRetired closes the hole the
// route-vs-spec comparison cannot see.
//
// A retired route is still a registered route: it answers 410 with a
// pointer to its replacement. To the presence check above that is
// indistinguishable from a live endpoint, so the spec kept describing
// `PUT .../parameters` as returning 200 with an UpdateParametersResponse
// long after the only way to write a parameter was the merge-patch route.
// An agent reading the spec found the write API that always fails and
// could not find the one that works.
func TestOpenAPIDrift_Admin_RetiredRoutesAreMarkedRetired(t *testing.T) {
	s := createTestNode(t, DefaultTestNodeConfig())
	spec := loadOpenAPI(t)

	var undocumented []string
	for _, r := range s.engine.Routes() {
		if !isPublicAdminPath(r.Path) || !isRetiredRoute(r.Handler) {
			continue
		}
		key := rk(r.Method, normalizeGinPath(r.Path))
		node, ok := spec.Paths[normalizeGinPath(r.Path)][strings.ToLower(r.Method)]
		if !ok {
			// Absent from the spec is fine: a route nobody can call does
			// not have to be described, only described honestly.
			continue
		}
		var op struct {
			Deprecated bool                 `yaml:"deprecated"`
			Responses  map[string]yaml.Node `yaml:"responses"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		if _, has410 := op.Responses["410"]; !op.Deprecated || !has410 {
			undocumented = append(undocumented, string(key))
		}
	}
	sort.Strings(undocumented)

	if len(undocumented) > 0 {
		t.Errorf("these routes only answer 410 but the spec still presents them as usable:\n  %s\n\n"+
			"Mark the operation `deprecated: true` and document the 410 (naming the replacement), "+
			"or drop it from openapi.yaml.",
			strings.Join(undocumented, "\n  "))
	}
}
