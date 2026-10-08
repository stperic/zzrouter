// operationId coverage for internal/server/openapi.yaml.
//
// operationId is the field code generators and agent tool-builders use
// to name the function they synthesize for an operation. When it is
// absent they fall back to inventing a name from the method and path,
// which collides across similar routes and changes shape between spec
// revisions. So the spec keeps two invariants:
//
//  1. Every operation declares an operationId.
//  2. No two operations share one.
//
// Shared helpers (loadOpenAPI, openAPIFile) come from
// openapi_drift_v1_test.go — same package, so they are visible here.

package server

import "testing"

// httpMethodKeys are the path-item keys that denote an operation.
// Everything else under a path (parameters, servers, $ref, summary) is
// not one and carries no operationId.
var httpMethodKeys = map[string]struct{}{
	"get": {}, "put": {}, "post": {}, "delete": {},
	"patch": {}, "head": {}, "options": {}, "trace": {},
}

func TestOpenAPI_EveryOperationHasAUniqueOperationID(t *testing.T) {
	spec := loadOpenAPI(t)

	seen := map[string]string{} // operationId → "METHOD path" that claimed it
	operations := 0

	for path, item := range spec.Paths {
		for method, node := range item {
			if _, ok := httpMethodKeys[method]; !ok {
				continue
			}
			operations++

			var op struct {
				OperationID string `yaml:"operationId"`
			}
			if err := node.Decode(&op); err != nil {
				t.Errorf("%s %s: decode operation: %v", method, path, err)
				continue
			}
			if op.OperationID == "" {
				t.Errorf("%s %s: missing operationId", method, path)
				continue
			}
			if prior, dup := seen[op.OperationID]; dup {
				t.Errorf("operationId %q is used by both %s and %s %s",
					op.OperationID, prior, method, path)
				continue
			}
			seen[op.OperationID] = method + " " + path
		}
	}

	if operations == 0 {
		t.Fatal("no operations found in openapi.yaml — the spec or this test is broken")
	}
	t.Logf("%d operations, %d distinct operationIds", operations, len(seen))
}
