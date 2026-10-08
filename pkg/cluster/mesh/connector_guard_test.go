package mesh

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every Connector that probes a peer must carry a cluster port.
//
// Worker admin ports bind 127.0.0.1 only (server_factory bind
// narrowing), so the plain-HTTP admin URL is unreachable from the
// coordinator. discoverVersion only tries the mTLS cluster URL when
// ClusterPort > 0; with a zero value it falls through to the admin URL
// and can reach no worker at all.
//
// That is not a slow path, it is a dead one: the endpoints built on a
// portless connector reported every healthy worker as unreachable while
// GET /nodes reported the same workers healthy. Two endpoints on one API
// disagreeing about the same fact.
//
// The bug was a sibling call site — the cluster-first ordering was added
// to discoverVersion but three constructors were left behind. This guard
// exists because that class of miss is invisible to any unit test of the
// connector itself, which is configured correctly by definition.
//
// connectorGuardExempt is a closed list, not a backlog. Adding a file
// asserts that its connector never probes a worker.
var connectorGuardExempt = map[string]string{
	"pkg/cluster/mesh/cluster.go": "defaults the connector for workers and degraded-mode callers, which have no cluster dispatch client to bake in",
}

func TestConnectorConfig_AlwaysCarriesClusterPort(t *testing.T) {
	root := moduleRoot(t)

	type violation struct {
		file string
		line int
	}
	var found []violation

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "vendor" || name == ".git" || strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if _, ok := connectorGuardExempt[rel]; ok {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Skip rather than fail: the walk covers the whole repo,
			// including testdata fixtures that are deliberately not
			// valid Go. A real source file that stopped parsing would
			// break the build long before this test ran.
			//nolint:nilerr // see above
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isNewConnector(call.Fun) || len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			if !hasField(lit, "ClusterPort") {
				found = append(found, violation{rel, fset.Position(call.Pos()).Line})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for _, v := range found {
		t.Errorf("%s:%d: NewConnector without ClusterPort — it cannot reach any worker, "+
			"because worker admin ports are loopback-only. Pass the coordinator's cluster port "+
			"(or reuse an already-configured Connector).", v.file, v.line)
	}
}

// isNewConnector matches both the in-package call and mesh.NewConnector.
func isNewConnector(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "NewConnector"
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		return ok && pkg.Name == "mesh" && f.Sel.Name == "NewConnector"
	}
	return false
}

func hasField(lit *ast.CompositeLit, name string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above working directory")
		}
		dir = parent
	}
}
