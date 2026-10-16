package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCauseAndRemedyToolsExistInCatalog(t *testing.T) {
	catalog := OperationBindings()
	toolName := regexp.MustCompile(`\bzz_[a-z][a-z0-9_]*\b`)
	// Inspect string literals so new causes and classes join this contract automatically.
	for _, path := range []string{
		"../../../pkg/prov_apps/process/memory_cause.go",
		"../../../pkg/httperr/engine_failure.go",
		"../failure_table.go",
	} {
		t.Run(path, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			require.NoError(t, err)
			count := 0
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				for _, name := range toolName.FindAllString(value, -1) {
					assert.Contains(t, catalog, name)
					count++
				}
				return true
			})
			assert.Positive(t, count)
		})
	}
}
