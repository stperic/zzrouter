package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Live run timestamps are mutable during daemon readiness. Response code must use the locked accessor.
func TestRunResponsesUseLockedTimestampContract(t *testing.T) {
	for _, name := range []string{"runs_executor.go", "logs_handlers.go"} {
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			field, ok := node.(*ast.SelectorExpr)
			if !ok || field.Sel.Name != "StartedAt" {
				return true
			}
			receiver, ok := field.X.(*ast.Ident)
			if ok && (receiver.Name == "inst" || receiver.Name == "run") {
				t.Errorf("%s: a live run timestamp must be read through GetStartedAt", positions.Position(field.Pos()))
			}
			return true
		})
	}
}
