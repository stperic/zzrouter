package detect_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// allowedDetectExports enumerates the complete exported surface of the
// detect package. Any new public symbol that lands must either belong
// here (because it's one of the two remaining probes or a trivial
// helper) or force a conscious update to this list — which is the
// reviewer's signal to ask "are we rebuilding the thing we just cut?".
var allowedDetectExports = map[string]bool{
	// The load-bearing spawn-path resolvers + ollama probes.
	"ProviderPython":        true,
	"ProviderCLI":           true,
	"ProbeOllama":           true,
	"OllamaVersionEndpoint": true,
	// Narrow port check kept from the old runtime.go.
	"CheckPortListening": true,
}

// TestDetectPackageExportedSurface asserts the exact set of exported
// identifiers in the detect package. Adding a `DispatchRule`, a
// `DetectProvider` façade, a `map[string]func(...)` dispatch table
// named publicly, or any struct named `Detector` / `Discovery` all
// trip this test. The shape of the old rules engine is deliberately
// out of bounds; this test makes "the shape" the thing we check,
// rather than any one syntactic pattern.
func TestDetectPackageExportedSurface(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	seen := map[string]string{}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil || d.Name == nil {
					continue
				}
				if name := d.Name.Name; ast.IsExported(name) {
					seen[name] = path
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(s.Name.Name) {
							seen[s.Name.Name] = path
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if ast.IsExported(n.Name) {
								seen[n.Name] = path
							}
						}
					}
				}
			}
		}
	}
	for name, path := range seen {
		if !allowedDetectExports[name] {
			t.Errorf("%s: exported symbol %q is not in allowedDetectExports — if this is intentional, add it and explain in the commit", path, name)
		}
	}
	for name := range allowedDetectExports {
		if _, ok := seen[name]; !ok {
			t.Errorf("allowedDetectExports lists %q but it was not found — surface drift, update the list", name)
		}
	}
}

// TestDetectPackageHasNoRulesEngine backs up the surface check with a
// shape check: no switch-on-Method, no loop-over-Detection slices
// beyond the single purpose-built OllamaVersionEndpoint helper.
func TestDetectPackageHasNoRulesEngine(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcName := ""
			if fn.Name != nil {
				funcName = fn.Name.Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch stmt := n.(type) {
				case *ast.SwitchStmt:
					if stmt.Tag == nil {
						return true
					}
					if sel, ok := stmt.Tag.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "Method" {
						t.Errorf("%s %s: switch on .Method (discovery rules engine resurrected)", fset.Position(n.Pos()), funcName)
					}
				case *ast.RangeStmt:
					// Iteration over Detection slices is only allowed in OllamaVersionEndpoint.
					if sel, ok := stmt.X.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == "Detection" {
						if funcName != "OllamaVersionEndpoint" {
							t.Errorf("%s %s: range over .Detection outside OllamaVersionEndpoint (rules engine resurrected)", fset.Position(n.Pos()), funcName)
						}
					}
				}
				return true
			})
		}
	}
}

// TestConnectivityExportsNoDetection asserts that the connectivity
// package does not export detection helpers. Detection used to live
// here alongside the HTTP connector; the cut moved every caller to
// install.ReadInstalledVersion / detect.ProbeOllama. Re-introducing
// a connectivity.CheckSomething / connectivity.ExtractSomething
// function would recreate the god-file the cut dismantled.
func TestConnectivityExportsNoDetection(t *testing.T) {
	banned := map[string]bool{
		"CheckProviderExists":             true,
		"GetProviderVersionFromDiscovery": true,
		"RuntimePythonExe":                true,
		"extractPythonModuleVersion":      true,
		"extractExecutableVersion":        true,
		"extractHTTPVersion":              true,
		"extractVersionWithPattern":       true,
		"extractValueFromJSONPath":        true,
		"findPythonExecutable":            true,
	}
	fset := token.NewFileSet()
	paths, err := filepath.Glob("../../connectivity/*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil {
				continue
			}
			if banned[fn.Name.Name] {
				t.Errorf("%s: banned detection symbol %q in pkg/connectivity", fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
}
