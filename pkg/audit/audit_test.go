package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestValidateRefusesIncompleteEvent refuses an event a review could not trust.
func TestValidateRefusesIncompleteEvent(t *testing.T) {
	t.Parallel()
	for name, ev := range map[string]Event{
		"missing action":  {Actor: "a", Subject: "s"},
		"unknown action":  {Action: "key.create", Actor: "a", Subject: "s"},
		"missing actor":   {Action: ActionKeyCreated, Subject: "s"},
		"missing subject": {Action: ActionKeyCreated, Actor: "a"},
	} {
		if err := ev.Validate(); err == nil {
			t.Errorf("%s: event accepted", name)
		}
	}
}

// TestActionConstants_WireFormat pins the stable on-the-wire string for
// every released Action constant. A rename here would invalidate recorded
// history and break compliance tooling that greps for these exact strings.
func TestActionConstants_WireFormat(t *testing.T) {
	want := map[Action]string{
		ActionKeyCreated:     "key.created",
		ActionKeyUpdated:     "key.updated",
		ActionKeyRotated:     "key.rotated",
		ActionKeyDeleted:     "key.deleted",
		ActionKeyUsageReset:  "key.usage_reset",
		ActionTeamCreated:    "team.created",
		ActionTeamUpdated:    "team.updated",
		ActionTeamDeleted:    "team.deleted",
		ActionTeamUsageReset: "team.usage_reset",
	}
	for got, expected := range want {
		if string(got) != expected {
			t.Errorf("Action wire value drift: %q != %q", string(got), expected)
		}
	}
	if ActorStaticAdmin != "static:admin" {
		t.Errorf("ActorStaticAdmin drift: %q", ActorStaticAdmin)
	}
}

func TestValidateRefusesMetadataJSONWouldAlter(t *testing.T) {
	t.Parallel()
	valid := Event{Action: ActionKeyCreated, Actor: "a", Subject: "s", Metadata: map[string]any{"purpose": "setup", "n": 1.5, "list": []any{"x"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid metadata refused: %v", err)
	}
	for name, metadata := range map[string]map[string]any{
		"invalid UTF-8 key":    {"bad\xffname": 1},
		"invalid UTF-8 nested": {"list": []any{map[string]any{"k": "bad\xff"}}},
		"NaN":                  {"n": math.NaN()},
		"unencodable":          {"c": make(chan int)},
	} {
		ev := valid
		ev.Metadata = metadata
		if err := ev.Validate(); err == nil {
			t.Errorf("%s: metadata accepted", name)
		}
	}
}

// TestActionsListsEveryConstant: Actions is the set an event records and a
// filter names, so an Action constant missing from it, in any file of the
// package and written either way, could never be recorded.
func TestActionsListsEveryConstant(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var declared, listed []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for _, value := range vs.Values {
						if lit := actionLiteral(vs.Type, value); lit != nil {
							text, err := strconv.Unquote(lit.Value)
							if err != nil {
								t.Fatal(err)
							}
							declared = append(declared, text)
						}
					}
				}
			}
		}
	}
	for _, action := range Actions() {
		listed = append(listed, string(action))
	}
	slices.Sort(declared)
	slices.Sort(listed)
	if len(declared) == 0 || !slices.Equal(declared, listed) {
		t.Errorf("Actions lists %v; the package declares %v", listed, declared)
	}
}

// actionLiteral is the string of a constant written X Action = "x" or
// X = Action("x"), and nil for any other constant.
func actionLiteral(typ, value ast.Expr) *ast.BasicLit {
	if call, ok := value.(*ast.CallExpr); ok && typ == nil && len(call.Args) == 1 {
		typ, value = call.Fun, call.Args[0]
	}
	ident, isAction := typ.(*ast.Ident)
	lit, isString := value.(*ast.BasicLit)
	if isAction && ident.Name == "Action" && isString && lit.Kind == token.STRING {
		return lit
	}
	return nil
}
