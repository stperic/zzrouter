package metadata_test

// Same layering guard as pkg/modelregistry/source/internal_import_guard_test.go
// and pkg/modelregistry/search/internal_import_guard_test.go: this
// peer package must not import the orchestrator root, because that
// would invert the dependency direction.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const forbiddenImport = "github.com/stperic/zzrouter/pkg/modelregistry"

// TestMetadataDoesNotImportOrchestrator walks every Go file under
// pkg/modelregistry/metadata and asserts none of them imports the
// orchestrator root package.
func TestMetadataDoesNotImportOrchestrator(t *testing.T) {
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
		t.Fatalf("walk pkg/modelregistry/metadata: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("metadata must not import the orchestrator:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
