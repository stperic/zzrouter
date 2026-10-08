package search_test

// This test lives under pkg/modelregistry/search so it can walk the
// package (and any future nested helpers) without importing it. It
// enforces the same layering invariant already covered by the
// exemplar guard at pkg/modelregistry/source/internal_import_guard_test.go:
// the peer must not reach back through the orchestrator root,
// because that would invert the dependency direction and one
// "convenient" helper-call away from an import cycle.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const forbiddenImport = "github.com/stperic/zzrouter/pkg/modelregistry"

// TestSearchDoesNotImportOrchestrator walks every Go file under
// pkg/modelregistry/search (recursive, includes _test.go) and
// asserts none of them imports the orchestrator root package.
// Deeper paths under pkg/modelregistry (metadata, source/*) stay
// permitted — those peers are leaves themselves.
func TestSearchDoesNotImportOrchestrator(t *testing.T) {
	t.Parallel()

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
			if p == forbiddenImport {
				violations = append(violations, path+": imports "+p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/modelregistry/search: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("search must not import the orchestrator:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
