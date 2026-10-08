package source_test

// This test lives under pkg/modelregistry/source so it can walk the
// source sub-packages without importing them. It enforces the layering
// invariant from the source-split plan: source sub-packages must not
// reach back through the orchestrator. Otherwise the direction of
// dependency reverses and we're one "convenient" helper-call away from
// an import cycle.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImport is the parent orchestrator package itself. Peer
// sub-packages (pkg/modelregistry/metadata, pkg/modelregistry/search,
// pkg/modelregistry/source/*) are permitted because none of them
// import the orchestrator — the hazard is ONLY the reverse-dependency
// on the root package.
const forbiddenImport = "github.com/stperic/zzrouter/pkg/modelregistry"

// TestSourceSubpackagesDoNotImportOrchestrator walks every Go file
// under pkg/modelregistry/source (recursive, includes _test.go) and
// asserts none of them imports the orchestrator root package. Any
// deeper path under pkg/modelregistry (metadata, search, source/*)
// is allowed — those peers are themselves leaves (they must not,
// and today do not, import the orchestrator).
func TestSourceSubpackagesDoNotImportOrchestrator(t *testing.T) {
	t.Parallel()

	// Walk from the current test's directory; Go's test runner sets cwd
	// to the package's source directory.
	root := "."

	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// This file itself lives under source/ — skip so its own
		// reference to forbiddenImport (as a string constant above)
		// does not appear in parsed imports anyway, but also skip
		// defensively.
		if strings.HasSuffix(path, "internal_import_guard_test.go") {
			return nil
		}

		af, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			violations = append(violations, path+": parse error: "+err.Error())
			return nil //nolint:nilerr // parse error is already surfaced via `violations`; keep walking
		}

		for _, imp := range af.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			// The only forbidden path is the orchestrator root itself.
			// Anything deeper under pkg/modelregistry/ (peers like
			// metadata, search, or sibling source sub-packages) stays
			// a leaf by its own construction and is permitted.
			if p == forbiddenImport {
				violations = append(violations, path+": imports "+p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/modelregistry/source: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("source sub-packages must not import the orchestrator:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
