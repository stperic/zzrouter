package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The client must call each path with a method the server accepts.
//
// TestAPIPaths_AllResolveToRegisteredRoutes checks that a path exists.
// That is not enough, and the gap was not theoretical: every parameter
// write in the TUI targeted a real path with a retired verb and got 410
// Gone. The path was fine. The method was the bug.
//
// This walks the client's call sites, pairs each HTTP method with the
// apipath builder it is used with, and checks the pair against the
// engine's method+path routing table.

// clientCallSiteDirs are the trees whose calls are checked.
var clientCallSiteDirs = []string{
	filepath.Join("..", "client"),
	filepath.Join("..", "cli"),
}

// conditionalRoutes are pairs the test engine cannot see.
//
// A test node builds with no provider config, so routes gated on
// configStore or providersService never register, and the engine here
// holds 383 routes where a configured node holds 384. Flagging those
// would report a bug in the client that does not exist.
//
// Closed list, and checked both ways: an entry that starts matching is
// reported as removable, so this cannot quietly become a graveyard.
// Verified against the running coordinator before being added.
var conditionalRoutes = map[string]string{
	"POST ProvidersInstances": "registers only when configStore != nil (providers_controller.go); " +
		"confirmed live, POST /providers/instances answers 400 not 404",
}

type methodPath struct {
	method string
	name   string // apipath identifier, e.g. "ProviderParameters"
	where  string
}

func TestClientCallSites_UseAnAcceptedMethod(t *testing.T) {
	s := createTestNodeWithDefaults(t)

	// method -> path templates the engine accepts for it.
	//
	// Retired routes are excluded, and that exclusion is the whole
	// reason this guard works. A tombstone is still a registered route,
	// so counting it as "accepted" made the check pass on exactly the
	// bug it exists to catch: reverting a write to the retired PUT was
	// invisible until these were filtered out. A 410 is not an accepted
	// method, it is a documented refusal.
	accepted := map[string][]string{}
	retired := 0
	for _, r := range s.engine.Routes() {
		if isRetiredRoute(r.Handler) {
			retired++
			continue
		}
		accepted[r.Method] = append(accepted[r.Method], r.Path)
	}
	if retired == 0 {
		t.Fatal("no retired routes found; the filter stopped matching and this guard is now blind to 410s")
	}
	t.Logf("excluded %d retired routes", retired)

	samples := apipathSamples()
	calls := collectClientCalls(t)
	if len(calls) == 0 {
		t.Fatal("found no client call sites; the scan is broken and this guard would pass vacuously")
	}
	t.Logf("checked %d client call sites", len(calls))

	var bad []string
	for _, c := range calls {
		concrete, ok := samples[c.name]
		if !ok {
			// Covered by TestAPIPathSamples_CoverEveryExportedPath.
			continue
		}
		pair := c.method + " " + c.name
		if matchesAnyTemplate(concrete, accepted[c.method]) {
			if _, listed := conditionalRoutes[pair]; listed {
				t.Errorf("conditionalRoutes has %q, but the test engine now routes it. "+
					"Remove the entry.", pair)
			}
			continue
		}
		if _, ok := conditionalRoutes[pair]; ok {
			continue
		}
		// Report which methods WOULD work: that is the fix, and
		// without it the failure says nothing actionable.
		var allowed []string
		for m, tmpls := range accepted {
			if matchesAnyTemplate(concrete, tmpls) {
				allowed = append(allowed, m)
			}
		}
		sort.Strings(allowed)
		detail := strings.Join(allowed, ", ")
		if detail == "" {
			detail = "none - the path is not routed at all"
		}
		bad = append(bad, c.method+" apipath."+c.name+" ("+c.where+"); server accepts: "+detail)
	}

	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("client calls %s. A wrong method on a real path is how every "+
			"parameter write ended up answering 410 Gone.", b)
	}
}

// collectClientCalls finds calls shaped f("METHOD", apipath.X) or
// f(ctx, "METHOD", apipath.X), which covers doJSON, doLongJSON,
// makeRequest and the streaming helpers without naming each one.
func collectClientCalls(t *testing.T) []methodPath {
	t.Helper()
	var out []methodPath

	for _, dir := range clientCallSiteDirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// Skip rather than fail: a source file that stopped
				// parsing breaks the build long before this test runs.
				//nolint:nilerr // see above
				return nil
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for i, arg := range call.Args {
					m, ok := httpMethodLiteral(arg)
					if !ok || i+1 >= len(call.Args) {
						continue
					}
					if name, ok := apipathIdent(call.Args[i+1]); ok {
						out = append(out, methodPath{
							method: m, name: name,
							where: filepath.Base(path) + ":" + strconv.Itoa(fset.Position(call.Pos()).Line),
						})
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

func httpMethodLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	switch v {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return v, true
	}
	return "", false
}

// apipathIdent pulls "X" out of apipath.X or apipath.X(...), including
// when the path is the head of a concatenation such as
// apipath.Runs + "?" + q.Encode().
func apipathIdent(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "apipath" {
			return v.Sel.Name, true
		}
	case *ast.CallExpr:
		return apipathIdent(v.Fun)
	case *ast.BinaryExpr:
		if v.Op == token.ADD {
			return apipathIdent(v.X)
		}
	}
	return "", false
}
