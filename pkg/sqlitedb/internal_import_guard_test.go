package sqlitedb_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestModerncImportsOnlyInSQLitedb(t *testing.T) {
	violations, err := moderncImportViolations(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}

func moderncImportViolations(root string) ([]string, error) {
	allowed := filepath.Join(root, "pkg", "sqlitedb")
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, "modernc.org/") && filepath.Dir(path) != allowed {
				violations = append(violations, fmt.Sprintf("%s imports %s outside pkg/sqlitedb", path, name))
			}
		}
		return nil
	})
	return violations, err
}

func TestModerncImportGuardSkipsIgnoredDirectories(t *testing.T) {
	for _, name := range []string{".hidden", "_ignored", "testdata", "vendor"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeImport := func(relative string) string {
				t.Helper()
				path := filepath.Join(root, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				source := []byte("package forbidden\nimport _ \"modernc.org/sqlite\"\n")
				if err := os.WriteFile(path, source, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			writeImport(filepath.Join("pkg", name, "driver.go"))
			writeImport(filepath.Join("pkg", "sqlitedb", "driver.go"))
			violations, err := moderncImportViolations(root)
			if err != nil || len(violations) != 0 {
				t.Fatalf("ignored or allowed source was checked: %v, %v", violations, err)
			}
			visible := writeImport(filepath.Join("pkg", "visible.go"))
			violations, err = moderncImportViolations(root)
			if err != nil || len(violations) != 1 || !strings.Contains(violations[0], visible) {
				t.Fatalf("ordinary source import was not caught: %v, %v", violations, err)
			}
		})
	}
}
