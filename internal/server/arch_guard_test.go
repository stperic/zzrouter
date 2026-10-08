package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchGuard_SetProviderEnabled_FunneledThroughFinalize enforces the
// funnel contract from docs/cluster_cache_coherency.md. Any direct
// configStore.SetProviderEnabled call in internal/server/ production code
// bypasses FinalizeOnboarding/FinalizeOffboarding and leaves the post-
// finalize hook (worker → coord notify) silent. The only allowed callers
// are the two funnel methods themselves.
func TestArchGuard_SetProviderEnabled_FunneledThroughFinalize(t *testing.T) {
	const allowedFile = "server_providers.go"
	allowedFuncs := map[string]bool{
		"FinalizeOnboarding":  true,
		"FinalizeOffboarding": true,
	}

	dir := "."
	fset := token.NewFileSet()

	var violations []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Don't recurse into nested packages.
			if path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		f, parseErr := parser.ParseFile(fset, path, src, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}

		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			funcName := fn.Name.Name

			ast.Inspect(fn, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SetProviderEnabled" {
					return true
				}
				base := filepath.Base(path)
				if base == allowedFile && allowedFuncs[funcName] {
					return true
				}
				pos := fset.Position(call.Pos())
				violations = append(violations,
					fmt.Sprintf("%s:%d — %s bypasses the FinalizeOn/Offboarding funnel", pos.Filename, pos.Line, funcName))
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("direct SetProviderEnabled calls must route through FinalizeOn/Offboarding:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
