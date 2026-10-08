package server

import (
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Documentation depth on the management surface.
//
// openapi_drift_admin_test.go answers "is the route in the spec". That
// stopped being the interesting question once both its ratchets reached
// zero: an operation can be present and still tell a client nothing,
// which is what 80 of the 160 operations did, with no description, and
// 64 did with no documented failure.
//
// An operation with no error response is the worse half. A client that
// reads only success shapes writes no error branch, and the first 409
// or 503 arrives as an unhandled case in production rather than as
// something the spec warned about.
//
// Both lists below are empty and must stay that way. A new operation
// fails this test until it says what it does and how it can fail.

// depthGapDescription and depthGapErrors are the waiver lists, kept as
// the mechanism this repo already uses for spec ratchets. Empty is the
// point: an entry here buys a green build by documenting less.
var (
	depthGapDescription = map[routeKey]struct{}{}
	depthGapErrors      = map[routeKey]struct{}{}
)

func TestOpenAPIDepth_ManagementOperationsExplainThemselves(t *testing.T) {
	spec := loadOpenAPI(t)

	var noDescription, noErrors, healedDesc, healedErr []string
	checked := 0

	for path, ops := range spec.Paths {
		if !isPublicAdminPath(path) {
			continue
		}
		for method, node := range ops {
			if method == "parameters" {
				continue
			}
			var op struct {
				Description string               `yaml:"description"`
				Responses   map[string]yaml.Node `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
			checked++
			key := rk(method, path)

			hasDesc := strings.TrimSpace(op.Description) != ""
			_, waivedDesc := depthGapDescription[key]
			switch {
			case !hasDesc && !waivedDesc:
				noDescription = append(noDescription, string(key))
			case hasDesc && waivedDesc:
				healedDesc = append(healedDesc, string(key))
			}

			hasErr := false
			for code := range op.Responses {
				if len(code) > 0 && (code[0] == '4' || code[0] == '5') {
					hasErr = true
					break
				}
			}
			_, waivedErr := depthGapErrors[key]
			switch {
			case !hasErr && !waivedErr:
				noErrors = append(noErrors, string(key))
			case hasErr && waivedErr:
				healedErr = append(healedErr, string(key))
			}
		}
	}

	if checked == 0 {
		t.Fatal("inspected no management operations; the spec load or the path filter broke")
	}
	t.Logf("checked %d management operations", checked)

	sort.Strings(noDescription)
	sort.Strings(noErrors)
	sort.Strings(healedDesc)
	sort.Strings(healedErr)

	if len(noDescription) > 0 {
		t.Errorf("these operations have a summary but no description, so a client "+
			"learns nothing a URL did not already tell it:\n  %s",
			strings.Join(noDescription, "\n  "))
	}
	if len(noErrors) > 0 {
		t.Errorf("these operations document no 4xx or 5xx response, so a client has "+
			"no reason to write an error branch:\n  %s",
			strings.Join(noErrors, "\n  "))
	}
	for _, k := range healedDesc {
		t.Errorf("%s now has a description; remove it from depthGapDescription", k)
	}
	for _, k := range healedErr {
		t.Errorf("%s now documents an error; remove it from depthGapErrors", k)
	}
}
