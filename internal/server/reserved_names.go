package server

import (
	"sort"
	"strconv"
	"strings"
)

// Reserved resource names.
//
// gin keeps one routing tree per HTTP method, and within a tree a
// literal segment beats a :param sibling. So a resource whose
// user-chosen name equals a literal sibling of its collection's :param
// route is created through the :param tree (no literal exists for the
// create verb) but then shadowed on every verb where a literal does
// exist: GET /zzrouter/v1/model-groups/schema always answers the schema
// endpoint, never a group named "schema".
//
// The sets below are the union of literal siblings across ALL methods,
// not just the ones shadowed today. A name colliding with any sibling
// is one route addition away from being stranded, and refusing it at
// create time costs the user nothing. The model-groups PUT exempts
// names that already exist (upsert grandfathering), so a pre-existing
// group is never locked out of updates by a later route addition.
//
// Deliberate bypasses: POST /state/restore rewrites the stores
// wholesale for round-trip fidelity, and provider config.yaml trees
// hand-authored on disk load without this check. Both are operator
// surfaces, not the API create path.
//
// TestReservedNames_CoverEveryShadowableCollection derives these sets
// from the live routing table; an entry here that the engine does not
// justify, or a route literal missing here, fails that test.
var (
	reservedModelGroupNames = reservedSet("schema", "events", "preview", "reload")
	reservedProviderNames   = reservedSet("catalog", "instances", "status")
	reservedTeamIDs         = reservedSet("schema")
	reservedKeyIDs          = reservedSet("schema", "reload")
)

func reservedSet(names ...string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

// reservedNameMessage is the one rejection sentence every surface
// emits, so all four collections stay equally helpful.
func reservedNameMessage(what, name, collection string, set map[string]bool) string {
	return what + " " + strconv.Quote(name) + " is reserved: it is a route segment under " +
		collection + ". Choose a name other than: " + reservedNameList(set)
}

// reservedNameList renders a set as a stable, comma-separated list for
// error messages.
func reservedNameList(set map[string]bool) string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
