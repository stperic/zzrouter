package httperr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// AllParamErrorCodes has to stay in step with the const block, or the
// thing it exists to prevent happens to it instead: the schema endpoint
// that advertises these codes to agents silently under-reports, and a
// client builds a local validator that treats a real code as unknown.
//
// So rather than trust the list, parse the declarations out of the
// source and compare. Adding a constant without listing it fails here.
func TestAllParamErrorCodesIsComplete(t *testing.T) {
	declared := declaredParamErrorCodes(t)
	listed := map[ParamErrorCode]bool{}
	for _, c := range AllParamErrorCodes() {
		if listed[c] {
			t.Errorf("AllParamErrorCodes lists %q twice", c)
		}
		listed[c] = true
	}

	for _, name := range sortedNames(declared) {
		code := declared[name]
		if !listed[code] {
			t.Errorf("constant %s (%q) is declared but missing from AllParamErrorCodes", name, code)
		}
	}

	byValue := map[ParamErrorCode]string{}
	for name, code := range declared {
		byValue[code] = name
	}
	for code := range listed {
		if _, ok := byValue[code]; !ok {
			t.Errorf("AllParamErrorCodes lists %q, which no constant declares", code)
		}
	}
}

// Wire values are a published contract: renaming one breaks every agent
// branching on it, so a change here should be a deliberate edit to this
// test rather than a silent pass.
func TestParamErrorCodeWireValuesAreStable(t *testing.T) {
	want := map[string]string{
		"CodeUnknownFlag":        "unknown_flag",
		"CodeUnknownField":       "unknown_field",
		"CodeWrongType":          "wrong_type",
		"CodeOutOfRange":         "out_of_range",
		"CodeUnknownNode":        "unknown_node",
		"CodeUnknownModel":       "unknown_model",
		"CodeCoercionFailed":     "coercion_failed",
		"CodeRetired":            "retired",
		"CodeRouteUnknown":       "route_unknown",
		"CodeReplicaUnknown":     "replica_unknown",
		"CodeReplicaLastInGroup": "replica_last_in_group",
		"CodeRouteNotClaimed":    "route_not_claimed",
		"CodeReservedName":       "reserved_name",
		"CodeRequired":           "required",
		"CodeInvalidValue":       "invalid_value",
		"CodePreflightFailed":    "preflight_failed",
		"CodeUnknownAsset":       "unknown_asset",
		"CodeAssetInUse":         "asset_in_use",
	}
	declared := declaredParamErrorCodes(t)
	if len(declared) != len(want) {
		t.Errorf("constant count changed: declared=%d pinned=%d — update this test deliberately",
			len(declared), len(want))
	}
	for name, code := range declared {
		expect, ok := want[name]
		if !ok {
			t.Errorf("new constant %s (%q) is not pinned here", name, code)
			continue
		}
		if string(code) != expect {
			t.Errorf("%s wire value changed: got %q, pinned %q", name, code, expect)
		}
	}
}

// declaredParamErrorCodes parses param_code.go and returns every
// constant declared with the ParamErrorCode type, keyed by Go name.
func declaredParamErrorCodes(t *testing.T) map[string]ParamErrorCode {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "param_code.go", nil, 0)
	if err != nil {
		t.Fatalf("parse param_code.go: %v", err)
	}

	out := map[string]ParamErrorCode{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only constants explicitly typed ParamErrorCode. An
			// untyped sibling in the same block is not part of the enum.
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "ParamErrorCode" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[name.Name] = ParamErrorCode(val)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no ParamErrorCode constants; the parser is broken, not the enum")
	}
	return out
}

func sortedNames(m map[string]ParamErrorCode) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
