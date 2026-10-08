package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestStartupReconnects_AreDeliveredAtCoordinatorStart pins the one
// call that makes mesh's held boot transitions land.
//
// NewCluster probes its peers before it returns, so a worker that was
// already up when this node started produces its Unknown->UP
// transition inside the constructor — where the reconnection callback
// cannot run, because the callback reaches back through a cluster
// handle the caller does not have yet. mesh holds those transitions;
// this call is what delivers them. Drop it and every consumer of
// ReconnectionCallback silently skips boot, which is exactly the class
// of bug that produced this test: the failure is invisible, because
// nothing errors and the next health tick papers over it minutes later.
func TestStartupReconnects_AreDeliveredAtCoordinatorStart(t *testing.T) {
	const (
		file    = "server_lifecycle.go"
		bootFn  = "startCoordinatorSubsystems"
		deliver = "ReplayStartupReconnects"
	)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var found, sawFn bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != bootFn {
			return true
		}
		sawFn = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == deliver {
				found = true
			}
			return !found
		})
		return false
	})

	if !sawFn {
		t.Fatalf("%s no longer declares %s: this guard needs rehoming, not deleting", file, bootFn)
	}
	if !found {
		t.Errorf("%s does not call %s: a peer that was already up at boot will never get its reconnect", bootFn, deliver)
	}
}
