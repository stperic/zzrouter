package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Error-envelope conformance, enforced statically.
//
// zzRouter serves three surfaces and each has its OWN error contract:
//
//	/zzrouter/v1/*  RFC 9457 application/problem+json, closed-enum code
//	/v1/*           OpenAI  {"error":{message,type,code,param}}
//	/api/*          Ollama  {"error":"..."}
//
// They differ deliberately: the compat surfaces have to match what the
// OpenAI and Ollama clients parse, so folding them into RFC 9457 would
// break every SDK. "Consistent" therefore cannot mean "identical" — it
// means each route emits its OWN surface's shape, every time.
//
// The failure this guards against is a handler bypassing its surface's
// responder and hand-rolling gin.H{"error": ...}. That produces a body
// no client of any surface knows how to read: no type, no code, no
// machine-actionable field. An agent gets a 500 it cannot classify.
//
// A live test cannot catch these reliably — most need a specific
// internal failure to trigger — so the check is static.

// errorEnvelopeExempt lists files allowed to build a bare
// {"error": ...} body, because on their surface that IS the contract.
//
// It is a closed list, not a backlog: anything not named here must go
// through a responder. Adding a file is a contract decision.
var errorEnvelopeExempt = map[string]string{
	"problem.go": "defines OllamaError — the Ollama surface's envelope is literally {\"error\": string}",
}

func TestErrorEnvelope_NoHandRolledErrorBodies(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)

	type violation struct {
		file string
		line int
		verb string
	}
	var found []violation

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		if _, ok := errorEnvelopeExempt[filepath.Base(path)]; ok {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			verb := sel.Sel.Name
			if verb != "JSON" && verb != "AbortWithStatusJSON" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			if !isErrorStatus(call.Args[0]) {
				return true
			}
			if !isBareErrorBody(call.Args[1]) {
				return true
			}
			found = append(found, violation{
				file: path,
				line: fset.Position(call.Pos()).Line,
				verb: verb,
			})
			return true
		})
	}

	if len(found) > 0 {
		var b strings.Builder
		b.WriteString("hand-rolled error bodies bypass their surface's responder.\n")
		b.WriteString("Use RespondWithProblem / RespondToError on /zzrouter/v1/*,\n")
		b.WriteString("OllamaError on /api/*, or the OpenAI responder on /v1/*.\n\n")
		for _, v := range found {
			b.WriteString("  " + v.file + ":" + strconv.Itoa(v.line) + " (" + v.verb + ")\n")
		}
		t.Error(b.String())
	}
}

// isErrorStatus reports whether the expression is a 4xx/5xx status,
// written either as http.StatusXxx or as a bare integer.
func isErrorStatus(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return false
		}
		code, ok := httpStatusByName[v.Sel.Name]
		return ok && code >= 400
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return false
		}
		n, err := strconv.Atoi(v.Value)
		return err == nil && n >= 400
	}
	return false
}

// isBareErrorBody reports whether the expression is an "error" body
// that belongs to no surface's contract.
//
// The distinction that matters: gin.H{"error": gin.H{"message":...,
// "type":...}} IS the OpenAI envelope and is correct on /v1/*. Only the
// flat form — gin.H{"error": "some string"} — belongs to nothing, and
// on the admin surface it displaces problem+json.
func isBareErrorBody(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		unquoted, err := strconv.Unquote(key.Value)
		if err != nil || unquoted != "error" {
			continue
		}
		return !isOpenAIErrorObject(kv.Value)
	}
	return false
}

// isOpenAIErrorObject reports whether the value is the nested OpenAI
// error object. Requiring BOTH message and type is deliberate: a bare
// map with only a message is not a shape any SDK classifies on.
func isOpenAIErrorObject(e ast.Expr) bool {
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	var hasMessage, hasType bool
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		switch name, _ := strconv.Unquote(key.Value); name {
		case "message":
			hasMessage = true
		case "type":
			hasType = true
		}
	}
	return hasMessage && hasType
}

// httpStatusByName maps the net/http status constant names this guard
// needs onto their codes. Only the error range matters; anything absent
// is treated as a non-error and skipped.
var httpStatusByName = map[string]int{
	"StatusBadRequest":            http.StatusBadRequest,
	"StatusUnauthorized":          http.StatusUnauthorized,
	"StatusPaymentRequired":       http.StatusPaymentRequired,
	"StatusForbidden":             http.StatusForbidden,
	"StatusNotFound":              http.StatusNotFound,
	"StatusMethodNotAllowed":      http.StatusMethodNotAllowed,
	"StatusNotAcceptable":         http.StatusNotAcceptable,
	"StatusRequestTimeout":        http.StatusRequestTimeout,
	"StatusConflict":              http.StatusConflict,
	"StatusGone":                  http.StatusGone,
	"StatusPreconditionFailed":    http.StatusPreconditionFailed,
	"StatusRequestEntityTooLarge": http.StatusRequestEntityTooLarge,
	"StatusUnprocessableEntity":   http.StatusUnprocessableEntity,
	"StatusTooManyRequests":       http.StatusTooManyRequests,
	"StatusMisdirectedRequest":    http.StatusMisdirectedRequest,
	"StatusInternalServerError":   http.StatusInternalServerError,
	"StatusNotImplemented":        http.StatusNotImplemented,
	"StatusBadGateway":            http.StatusBadGateway,
	"StatusServiceUnavailable":    http.StatusServiceUnavailable,
	"StatusGatewayTimeout":        http.StatusGatewayTimeout,
}
