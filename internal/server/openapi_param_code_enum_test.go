package server

import (
	"sort"
	"testing"

	"github.com/stperic/zzrouter/pkg/httperr"
)

// The ParamError code enum in openapi.yaml must equal
// httperr.AllParamErrorCodes().
//
// Before this test existed the spec enum was five codes stale: an agent
// validating responses against the spec would treat route_unknown and
// friends as unrecognized values. The Go enum is the source of truth
// (its own tests pin completeness and wire stability), so the spec is
// checked against it, not the other way around.
func TestOpenAPIParamErrorEnum_MatchesDeclaredCodes(t *testing.T) {
	spec := loadOpenAPI(t)
	got := spec.Components.Schemas.ParamError.Properties.Code.Enum
	if len(got) == 0 {
		t.Fatal("openapi.yaml has no ParamError.properties.code.enum; the schema moved and this test went blind")
	}

	want := map[string]bool{}
	for _, c := range httperr.AllParamErrorCodes() {
		want[string(c)] = true
	}
	seen := map[string]bool{}
	for _, c := range got {
		if !want[c] {
			t.Errorf("openapi.yaml ParamError enum lists %q, which pkg/httperr does not declare", c)
		}
		seen[c] = true
	}
	var missing []string
	for c := range want {
		if !seen[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	for _, c := range missing {
		t.Errorf("pkg/httperr declares %q but the openapi.yaml ParamError enum omits it; "+
			"agents validating against the spec will treat it as unrecognized", c)
	}
}
