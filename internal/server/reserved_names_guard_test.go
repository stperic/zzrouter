package server

import (
	"sort"
	"strings"
	"testing"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// Reserved-name coverage.
//
// reserved_names.go declares, per collection, the literal route segments
// a user-chosen name must not equal. This test derives the same
// information from the live routing table, both ways: a literal the
// engine registers that the reserved set misses is a hole (the next
// /nodes/compatible), and a reserved entry the engine no longer
// justifies is stale. Collections with literal siblings but no reserved
// set must appear in shadowExemptPrefixes with a reason, so a new
// shadow-prone collection cannot appear silently.
//
// Tombstone routes are deliberately INCLUDED: a 410 handler is still a
// registered literal, and it shadows exactly like a live one.

// guardedPrefixes maps each collection prefix (the path up to and
// excluding the :param segment) to the reserved set enforced for it.
var guardedPrefixes = map[string]map[string]bool{
	apipath.Base + "/model-groups": reservedModelGroupNames,
	apipath.Base + "/providers":    reservedProviderNames,
	apipath.Base + "/teams":        reservedTeamIDs,
	apipath.Base + "/keys":         reservedKeyIDs,
}

// shadowExemptPrefixes lists collections where literal siblings of a
// :param exist but no reserved-name check is enforced. Every entry
// needs a reason; an entry that stops matching a shadowable collection
// is reported as removable.
var shadowExemptPrefixes = map[string]string{
	apipath.Base + "/runs": "run ids are server-generated, never user-chosen",
	apipath.Base + "/pricing": "overrides are keyed by (provider, model) in body/query by design " +
		"(model ids carry slashes); only the convenience lookup GET is shadowed, and model ids " +
		"come from upstream catalogs, not user creation",
	apipath.Base + "/nodes": "node names arrive via cluster pairing (node.yaml on the worker), " +
		"not an API create; rejecting them at pairing is a cluster-protocol change",
	apipath.Base + "/inference-logs": "log entry ids are server-generated, never user-chosen",
}

func TestReservedNames_CoverEveryShadowableCollection(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{
		AdminKey:         TestAdminKey,
		SeedProvidersDir: true, // registers POST /providers/instances, whose literal must be derivable
	})

	type collection struct {
		params   map[string]bool
		literals map[string]bool
	}
	cols := map[string]*collection{}
	for _, r := range s.engine.Routes() {
		// Scope: the management API only. Compat surfaces (/v1, /api) are
		// frozen by the OpenAI and Ollama contracts and create nothing
		// name-addressable; /internal/ params name existing resources.
		if !strings.HasPrefix(r.Path, apipath.Base+"/") ||
			strings.HasPrefix(r.Path, apipath.Base+"/internal/") {
			continue
		}
		segs := strings.Split(r.Path, "/")
		for i := 1; i < len(segs); i++ {
			prefix := strings.Join(segs[:i], "/")
			col := cols[prefix]
			if col == nil {
				col = &collection{params: map[string]bool{}, literals: map[string]bool{}}
				cols[prefix] = col
			}
			if strings.HasPrefix(segs[i], ":") || strings.HasPrefix(segs[i], "*") {
				col.params[segs[i]] = true
			} else {
				col.literals[segs[i]] = true
			}
		}
	}

	seenExempt := map[string]bool{}
	for prefix, col := range cols {
		if len(col.params) == 0 || len(col.literals) == 0 {
			continue // not shadowable: no param, or no literal competing with it
		}
		if reserved, ok := guardedPrefixes[prefix]; ok {
			for lit := range col.literals {
				if !reserved[lit] {
					t.Errorf("%s: literal %q is a sibling of %v but is not in the reserved set; "+
						"a resource created with that name is (or is one route away from being) unreachable. "+
						"Add it in reserved_names.go.", prefix, lit, segNamesOf(col.params))
				}
			}
			for name := range reserved {
				if !col.literals[name] {
					t.Errorf("%s: reserved name %q no longer matches any route literal; "+
						"remove it from reserved_names.go.", prefix, name)
				}
			}
			continue
		}
		if _, ok := shadowExemptPrefixes[prefix]; ok {
			seenExempt[prefix] = true
			continue
		}
		t.Errorf("%s: has both %v and literals %v but neither a reserved set nor an exemption. "+
			"If its names are user-chosen this is the next /nodes/compatible; add a reserved set "+
			"in reserved_names.go, or an exemption here with the reason names cannot collide.",
			prefix, segNamesOf(col.params), segNamesOf(col.literals))
	}

	for prefix := range shadowExemptPrefixes {
		if !seenExempt[prefix] {
			t.Errorf("shadowExemptPrefixes has %q, but the engine has no shadowable collection there. "+
				"Remove the entry.", prefix)
		}
	}
	for prefix := range guardedPrefixes {
		col := cols[prefix]
		if col == nil || len(col.params) == 0 || len(col.literals) == 0 {
			t.Errorf("guardedPrefixes has %q, but the engine has no shadowable collection there. "+
				"Remove the entry or fix the prefix.", prefix)
		}
	}
}

// TestRedirectFixedPathStaysOff pins the engine setting the reserved
// sets rely on being exact-match: with RedirectFixedPath on, gin
// case-folds "Schema" onto the literal route, and every reserved set
// would silently need case-insensitive matching.
func TestRedirectFixedPathStaysOff(t *testing.T) {
	s := createTestNodeWithDefaults(t)
	if s.engine.RedirectFixedPath {
		t.Fatal("RedirectFixedPath is on; reserved-name checks are exact-match and assume it stays off")
	}
}

func segNamesOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
