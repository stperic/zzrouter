package access_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Walk production imports on every platform, including transitive module peers.
// Test-only dependencies do not become part of the client or launcher binary.
func TestClientsDoNotImportAccessStorage(t *testing.T) {
	t.Parallel()
	const module = "github.com/stperic/zzrouter/"
	storage := []string{module + "pkg/access", module + "pkg/access/filestore", module + "pkg/access/people", module + "pkg/access/sqlstore", module + "pkg/sqlitedb"}
	root := filepath.Join("..", "..")
	roots := []string{"cmd/zzrouter", "cmd/zzrouter-launcher"}
	for _, subtree := range []string{"internal/cli/clientcli", "internal/client"} {
		err := filepath.WalkDir(filepath.Join(root, subtree), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				roots = append(roots, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	imports := make(map[string][]string)
	for _, client := range roots {
		seen := make(map[string]bool)
		var walk func(string, []string)
		walk = func(pkg string, chain []string) {
			chain = append(slices.Clone(chain), pkg)
			// The root holds the Store interface and imports people.
			if slices.Contains(storage, pkg) || strings.HasPrefix(pkg, "modernc.org/") {
				t.Errorf("client or launcher imports access storage: %s", strings.Join(chain, " -> "))
				return
			}
			if seen[pkg] {
				return
			}
			seen[pkg] = true
			deps, loaded := imports[pkg]
			if !loaded {
				dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, module)))
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatalf("read import chain %s: %v", strings.Join(chain, " -> "), err)
				}
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
						continue
					}
					file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
					if err != nil {
						t.Fatal(err)
					}
					for _, imp := range file.Imports {
						dep, err := strconv.Unquote(imp.Path.Value)
						if err != nil {
							t.Fatal(err)
						}
						if strings.HasPrefix(dep, module) || strings.HasPrefix(dep, "modernc.org/") {
							deps = append(deps, dep)
						}
					}
				}
				imports[pkg] = deps
			}
			for _, dep := range deps {
				walk(dep, chain)
			}
		}
		walk(module+client, nil)
	}
}

// Only the access store names its database file, so nothing else opens it.
func TestOnlyTheAccessStoreNamesItsDatabase(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	allowed := filepath.Join(root, "pkg", "access", "sqlstore")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == allowed {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if value, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(value, "access.sqlite") {
					t.Errorf("%s names access.sqlite outside pkg/access/sqlstore", path)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
