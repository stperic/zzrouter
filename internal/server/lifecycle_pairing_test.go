package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLifecycle_StartStopPairing enforces plan Principle #2: every
// subsystem Start call in a start*Subsystems group has a matching
// Stop call in the paired stop*Subsystems group. Prevents silent
// drift where future Start calls land without a matching Stop —
// critical for the role-aware mode switch, where a demote invokes
// stopCoordinatorSubsystems and any orphan Start leaks a goroutine.
//
// Exemptions cover deliberate design choices, documented at the call
// site. Additions require updating the exemption map with a reason.
func TestLifecycle_StartStopPairing(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server_lifecycle.go", nil, parser.ParseComments)
	require.NoError(t, err)

	groups := []struct {
		start, stop string
	}{
		{"startCoordinatorSubsystems", "stopCoordinatorSubsystems"},
		{"startCommonSubsystems", "stopCommonSubsystems"},
	}

	for _, g := range groups {
		t.Run(g.start+"/"+g.stop, func(t *testing.T) {
			started := methodTargets(t, file, g.start, "Start", "Warm")
			stopped := methodTargets(t, file, g.stop, "Stop")

			// Every started subsystem must have a Stop — unless exempt.
			for recv := range started {
				if _, ok := stopped[recv]; ok {
					continue
				}
				if reason, exempt := startWithoutStopExempt[recv]; exempt {
					t.Logf("exempt: %s has Start without Stop — %s", recv, reason)
					continue
				}
				t.Errorf("%s calls %s.Start but %s has no matching Stop", g.start, recv, g.stop)
			}

			// Orphan Stops (Stop without Start) are closer-pattern: allowed but
			// must be declared. Undocumented orphans fail.
			for recv := range stopped {
				if _, ok := started[recv]; ok {
					continue
				}
				if reason, exempt := stopWithoutStartExempt[recv]; exempt {
					t.Logf("exempt: %s has Stop without Start — %s", recv, reason)
					continue
				}
				t.Errorf("%s calls %s.Stop but %s has no matching Start", g.stop, recv, g.start)
			}
		})
	}
}

// startWithoutStopExempt lists receiver chains that legitimately call
// Start (or Warm) without a paired Stop in the same group.
var startWithoutStopExempt = map[string]string{
	"s.updateScheduler": "constructed lazily inside startCommonSubsystems; Stop is unconditional",
	"s.updateRollouts":  "coordinator-only durable update owner; Stop is unconditional",
	"s.maintenance":     "constructed lazily inside startCommonSubsystems; Stop is unconditional",
}

// stopWithoutStartExempt lists receiver chains that legitimately have
// a Stop with no paired Start in the same group.
var stopWithoutStartExempt = map[string]string{
	"s.inference.logStore": "closer-pattern: ring buffer has no goroutine, Stop drains subscribers",
	"s.model.Downloads":    "closer-pattern: tracker persisted lazily, Stop performs final flush",
	"s.jobs":               "owned-by-factory: registry is constructed in server_factory (before startCommonSubsystems runs) and launches its own janitor goroutine; Stop joins it",
}

// methodTargets walks the body of the named function and returns the
// set of receiver chains (e.g. "s.access", "s.providers.fallback")
// on which one of the listed methods is called.
func methodTargets(t *testing.T, file *ast.File, fnName string, methods ...string) map[string]struct{} {
	t.Helper()
	fn := findFunc(file, fnName)
	require.NotNilf(t, fn, "expected function %s in server_lifecycle.go", fnName)

	want := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		want[m] = struct{}{}
	}

	out := map[string]struct{}{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if _, wanted := want[sel.Sel.Name]; !wanted {
			return true
		}
		chain := receiverChain(sel.X)
		if chain == "" {
			return true
		}
		out[chain] = struct{}{}
		return true
	})
	return out
}

// findFunc returns the top-level method with the given name, or nil.
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// receiverChain renders a selector chain (s.foo.bar) back to source.
// Returns "" if the expression is not a pure selector rooted at an ident.
func receiverChain(expr ast.Expr) string {
	var parts []string
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			parts = append(parts, e.Name)
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return strings.Join(parts, ".")
		case *ast.SelectorExpr:
			parts = append(parts, e.Sel.Name)
			expr = e.X
		default:
			return ""
		}
	}
}
