package server

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stperic/zzrouter/pkg/apipath"
)

// Tag taxonomy (plan C8).
//
// The rule is mechanical so an agent can predict a route's group from
// its path: one tag per first path segment, with duplicate resources
// (plan C3) sharing their canonical twin's tag so the pairs slated for
// convergence sit in one group. "System" was a bucket, not a group,
// and is gone.
//
// tagBySegment is the taxonomy. It deliberately covers LIVE segments
// that are not yet documented (config, server, state, ...): when their
// operations land in the spec, the tag is already decided, and the
// live-coverage check below fails on any new segment nobody placed.
var tagBySegment = map[string]string{
	"cache":          "Cache",
	"cluster":        "Cluster",
	"config":         "Config",
	"deployments":    "Deployments",
	"discover":       "Discovery",
	"health":         "Health",
	"inference-logs": "InferenceLogs",
	"jobs":           "Jobs",
	"keys":           "Keys",
	"model-groups":   "ModelGroups",
	"models":         "Models",
	"nodes":          "Nodes",
	"pricing":        "Pricing",
	"providers":      "Providers",
	"registries":     "Registries",
	"runs":           "Runs",
	"search":         "Search",
	"server":         "Server",
	"spend":          "Spend",
	"teams":          "Teams",
	"update":         "Update",
	"usage":          "Usage",
	"state":          "State",
	"system":         "System",

	// C3 resolved (arc 1.4): the former duplicate segments moved under
	// /server or /providers; /system keeps its own segment because it is
	// cluster-capable, unlike the node-local /server family.
}

// tagByExactPath covers paths the segment rule cannot reach: surface
// indexes and the self-description endpoints.
var tagByExactPath = map[string]string{
	"/openapi.yaml":          "Spec",
	"/metrics":               "Metrics",
	"/openapi.json":          "Spec",
	apipath.Base:             "Spec", // the management-API index
	"/v1":                    "OpenAICompat",
	apipath.Base + "/health": "Health",
}

func expectedTagForPath(path string) (string, bool) {
	if tag, ok := tagByExactPath[path]; ok {
		return tag, true
	}
	switch {
	case strings.HasPrefix(path, apipath.Base+"/"):
		seg := strings.SplitN(strings.TrimPrefix(path, apipath.Base+"/"), "/", 2)[0]
		tag, ok := tagBySegment[seg]
		return tag, ok
	case path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/"):
		return "AnthropicCompat", true
	case strings.HasPrefix(path, "/v1/"):
		return "OpenAICompat", true
	case strings.HasPrefix(path, "/api/"):
		return "OllamaCompat", true
	case path == "/health" || strings.HasPrefix(path, "/health/"):
		return "Health", true
	}
	return "", false
}

// TestOpenAPITags_FollowTheSegmentTaxonomy checks the documented
// surface: every operation carries exactly one tag, and it is the tag
// the taxonomy derives from the path.
func TestOpenAPITags_FollowTheSegmentTaxonomy(t *testing.T) {
	spec := loadOpenAPI(t)
	if len(spec.Paths) == 0 {
		t.Fatal("openapi.yaml has no paths; the loader went blind")
	}
	for path, ops := range spec.Paths {
		want, ok := expectedTagForPath(path)
		if !ok {
			t.Errorf("%s: no taxonomy entry derives a tag for this path; add its segment to tagBySegment "+
				"or the path to tagByExactPath", path)
			continue
		}
		for method, tags := range operationTags(t, path, ops) {
			if len(tags) != 1 {
				t.Errorf("%s %s: has %d tags %v, want exactly one (predictability beats cross-listing)",
					method, path, len(tags), tags)
				continue
			}
			if tags[0] != want {
				t.Errorf("%s %s: tagged %q, taxonomy says %q", method, path, tags[0], want)
			}
		}
	}
}

// TestOpenAPITags_DeclaredMatchesUsed keeps the top-level tags block
// honest in both directions: a declared-but-unused tag is dead weight
// an agent will chase, and a used-but-undeclared tag renders untitled
// in every spec viewer. Taxonomy entries for still-undocumented
// segments (Config, State) are deliberately NOT declared here; they
// enter the spec with their first documented operation.
func TestOpenAPITags_DeclaredMatchesUsed(t *testing.T) {
	spec := loadOpenAPI(t)
	declared := map[string]bool{}
	for _, tag := range spec.Tags {
		declared[tag.Name] = true
	}
	if len(declared) == 0 {
		t.Fatal("openapi.yaml declares no tags; the loader went blind")
	}
	used := map[string]bool{}
	for path, ops := range spec.Paths {
		for _, tags := range operationTags(t, path, ops) {
			for _, tag := range tags {
				used[tag] = true
			}
		}
	}
	for tag := range used {
		if !declared[tag] {
			t.Errorf("tag %q is used but not declared in the top-level tags block", tag)
		}
	}
	for tag := range declared {
		if !used[tag] {
			t.Errorf("tag %q is declared but no operation uses it; remove it or tag something with it", tag)
		}
	}
}

// TestOpenAPITags_TaxonomyCoversTheLiveSurface walks the running
// engine, not the spec: every first segment of the management API must
// have a taxonomy entry, documented or not. This is what makes the
// taxonomy "cover all resources" instead of covering the spec's
// current subset, and it fails the moment a new collection is routed
// without anyone deciding its group.
func TestOpenAPITags_TaxonomyCoversTheLiveSurface(t *testing.T) {
	s := createTestNode(t, TestNodeConfig{AdminKey: TestAdminKey, SeedProvidersDir: true})

	liveSegs := map[string]bool{}
	for _, r := range s.engine.Routes() {
		if !strings.HasPrefix(r.Path, apipath.Base+"/") ||
			strings.HasPrefix(r.Path, apipath.Base+"/internal/") {
			continue
		}
		liveSegs[strings.SplitN(strings.TrimPrefix(r.Path, apipath.Base+"/"), "/", 2)[0]] = true
	}
	if len(liveSegs) == 0 {
		t.Fatal("derived no live segments; the route walk is broken and this guard would pass vacuously")
	}

	var missing []string
	for seg := range liveSegs {
		if _, ok := tagBySegment[seg]; !ok {
			missing = append(missing, seg)
		}
	}
	sort.Strings(missing)
	for _, seg := range missing {
		t.Errorf("live segment %q has no taxonomy entry; decide its tag in tagBySegment before its "+
			"operations get documented under an accidental one", seg)
	}

	for seg := range tagBySegment {
		if !liveSegs[seg] {
			t.Errorf("taxonomy maps %q, but the live engine routes no such segment; remove the stale entry", seg)
		}
	}
}

// operationTags decodes the tags of every HTTP operation under a path
// item, skipping non-operation keys such as "parameters".
func operationTags(t *testing.T, path string, ops map[string]yaml.Node) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for method, node := range ops {
		switch method {
		case "get", "put", "post", "delete", "patch", "head", "options":
		default:
			continue
		}
		var op struct {
			Tags []string `yaml:"tags"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatalf("%s %s: decode operation: %v", method, path, err)
		}
		out[method] = op.Tags
	}
	return out
}
