// Request binding and validation helpers.
//
// This file centralizes JSON body binding and validator error formatting so
// that:
//
//  1. Validation errors report JSON field names (e.g. "launch_mode") instead
//     of leaking Go struct field names (e.g. "LaunchMode"). This is done once,
//     at startup, by registering a TagNameFunc on go-playground/validator.
//
//  2. Validation failures produce a machine-readable `errors` array on the
//     Problem Details response. Each entry carries {field, rule, param,
//     message} so API clients — especially AI agents — can react to failures
//     programmatically (e.g. read the valid values for a `oneof` tag rather
//     than parsing the human-readable Detail string).
//
// Handlers should call BindJSON instead of ShouldBindJSON directly:
//
//	if !BindJSON(c, &req) {
//	    return
//	}
//
// BindJSON writes the Problem Details response on failure and returns false.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/utils"
)

// RegisterValidatorTranslator configures the go-playground validator used by
// Gin so that error reports reference JSON field names instead of Go struct
// field names.
//
// This MUST be called once at startup, before any request is handled.
//
// Before: "Field validation for 'LaunchMode' failed on the 'required' tag"
// After:  "launch_mode is required"
func RegisterValidatorTranslator() {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		// Validator engine is not the standard one; nothing to do.
		return
	}
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		raw := fld.Tag.Get("json")
		name := strings.SplitN(raw, ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
}

// BindJSON binds the JSON request body to req and runs validation. On failure
// it writes a 400 Problem Details response with a machine-readable `errors`
// array and returns false; on success it returns true.
//
// The request body is consumed; do not call c.ShouldBindJSON again afterward.
func BindJSON(c *gin.Context, req any) bool {
	err := c.ShouldBindJSON(req)
	if err == nil {
		return true
	}
	writeBindError(c, err)
	return false
}

// BindJSONOptional is like BindJSON but treats an empty body (io.EOF) as a
// valid no-op, leaving req at its zero value. Use this for endpoints whose
// request body is optional (e.g. POST actions with no parameters).
func BindJSONOptional(c *gin.Context, req any) bool {
	err := c.ShouldBindJSON(req)
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	writeBindError(c, err)
	return false
}

// BindJSONStrict is BindJSON for the public management API: a field the
// DTO does not declare is a 400 rather than a silent no-op. A caller that
// misspells a field, or sets one this resource does not have, otherwise
// gets a 2xx and believes the value took effect.
//
// Compat (/v1/*, /api/*) and cluster-internal traffic must stay lenient —
// SDK clients send parameters we do not model, and a peer one patch
// release ahead may send a field this build has not learned yet. Those
// surfaces keep BindJSON.
func BindJSONStrict(c *gin.Context, req any) bool {
	return bindStrict(c, req, false)
}

// BindJSONStrictOptional is BindJSONStrict with BindJSONOptional's
// empty-body allowance, for management endpoints whose body carries only
// optional fields.
func BindJSONStrictOptional(c *gin.Context, req any) bool {
	return bindStrict(c, req, true)
}

// bindStrict decodes with DisallowUnknownFields and then runs the same
// validator gin would have run, so strict and lenient binding report
// validation failures identically.
func bindStrict(c *gin.Context, req any, allowEmpty bool) bool {
	if c.Request == nil || c.Request.Body == nil {
		if allowEmpty {
			return true
		}
		writeBindError(c, io.EOF)
		return false
	}

	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	switch err := dec.Decode(req); {
	case err == nil:
	case errors.Is(err, io.EOF) && allowEmpty:
		return true
	default:
		writeBindError(c, err)
		return false
	}
	if dec.More() {
		writeBindError(c, errTrailingJSON)
		return false
	}

	// gin's own decodeJSON guards this before validating; a nil engine is
	// possible when binding.Validator has been swapped out.
	if binding.Validator != nil {
		if err := binding.Validator.ValidateStruct(req); err != nil {
			writeBindError(c, err)
			return false
		}
	}
	return true
}

// unknownFieldPrefix is what encoding/json puts in front of the quoted
// field name when DisallowUnknownFields trips. It tracks that package's
// wording because the error is untyped; bindStrict decodes with
// encoding/json directly, so this stays in step as long as it does.
const unknownFieldPrefix = "json: unknown field "

// unknownFieldName reports the field name (still quoted, as encoding/json
// wrote it) when err is a DisallowUnknownFields rejection. Shared so the
// two strict decoders in this package recognise it the same way.
func unknownFieldName(err error) (string, bool) {
	return strings.CutPrefix(err.Error(), unknownFieldPrefix)
}

// errTrailingJSON is the rejection for a body carrying more than one JSON
// document. encoding/json stops at the first, so without this a caller
// that sent two objects has the second silently dropped -- the same
// report-success-for-nothing failure DisallowUnknownFields exists to stop.
var errTrailingJSON = errors.New("body must contain a single JSON object")

// writeBindError formats a binding error as a Problem Details response.
// Validator errors produce a structured `errors` array; other errors (e.g.
// JSON syntax errors) produce a plain Detail message.
func writeBindError(c *gin.Context, err error) {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		// Same responder as every other per-key failure, so a body
		// rejection and a parameter rejection are one shape for a
		// caller. formatValidationDetail keeps this path's own prose
		// summary, which reads better than a join of the messages.
		RespondWithParamErrorsDetail(c, http.StatusBadRequest, "Bad Request",
			formatValidationDetail(ve), buildFieldErrors(ve))
		return
	}
	// JSON syntax, type, or unknown binding error — do not leak internal types.
	var syntaxErr *json.SyntaxError
	var unmarshalTypeErr *json.UnmarshalTypeError
	switch name, isUnknownField := unknownFieldName(err); {
	case isUnknownField:
		// encoding/json reports this as an untyped error, so the prefix is
		// the only handle. It already quotes the field name.
		BadRequest(c, fmt.Sprintf("unknown field %s: this endpoint rejects fields it does not define", name))
	case errors.Is(err, errTrailingJSON):
		BadRequest(c, err.Error())
	case errors.As(err, &syntaxErr):
		BadRequest(c, fmt.Sprintf("invalid JSON syntax at offset %d", syntaxErr.Offset))
	case errors.As(err, &unmarshalTypeErr):
		BadRequest(c, fmt.Sprintf("invalid type for field %q: expected %s",
			unmarshalTypeErr.Field, unmarshalTypeErr.Type.String()))
	case errors.Is(err, io.EOF):
		BadRequest(c, "request body is required")
	default:
		BadRequest(c, "invalid request body")
	}
}

// buildFieldErrors converts validator errors into per-key entries keyed
// by JSON field name (requires RegisterValidatorTranslator).
//
// The validator tag is mapped onto the closed ParamErrorCode vocabulary
// rather than reported raw. Raw tags were an open string in a field
// called `rule`, which left the management surface with two vocabularies
// for one idea: a client branching on `code` for a bad provider
// parameter had to branch on `rule` for a bad request body.
//
// The offending value is deliberately NOT echoed in Got. A field that
// fails `required` or `email` may be a credential, and an error body is
// the last place to reprint one.
func buildFieldErrors(ve validator.ValidationErrors) []utils.ParamError {
	out := make([]utils.ParamError, 0, len(ve))
	for _, fe := range ve {
		code, want, min, max := classifyValidatorTag(fe)
		out = append(out, utils.ParamError{
			Key:     fe.Field(),
			Code:    string(code),
			Want:    want,
			Min:     min,
			Max:     max,
			Message: humanizeFieldError(fe),
		})
	}
	return out
}

// classifyValidatorTag maps a go-playground tag onto the closed code
// vocabulary, carrying the tag's parameter into the field that makes it
// actionable: a bound becomes Min or Max, anything else becomes Want.
//
// Unknown tags land on invalid_value rather than on a code of their own.
// A tag nobody mapped is still a value the caller may not send, and
// answering with a code no client recognises would be worse than
// answering with a broad one that every client already handles.
func classifyValidatorTag(fe validator.FieldError) (code httperr.ParamErrorCode, want string, min, max any) {
	param := fe.Param()
	switch fe.Tag() {
	case "required":
		return httperr.CodeRequired, "", nil, nil
	case "min", "gte":
		return httperr.CodeOutOfRange, "", param, nil
	case "max", "lte":
		return httperr.CodeOutOfRange, "", nil, param
	case "gt":
		return httperr.CodeOutOfRange, "greater than " + param, nil, nil
	case "lt":
		return httperr.CodeOutOfRange, "less than " + param, nil, nil
	case "len":
		return httperr.CodeOutOfRange, "exactly " + param, param, param
	case "oneof":
		return httperr.CodeInvalidValue, "one of: " + strings.ReplaceAll(param, " ", ", "), nil, nil
	default:
		// email, url, uuid and anything else a struct tag introduces
		// later: the tag names the expectation well enough to be Want.
		return httperr.CodeInvalidValue, fe.Tag(), nil, nil
	}
}

// formatValidationDetail produces a concise human-readable summary that an
// operator or developer would find useful in logs or a generic error UI. For
// programmatic use, clients should read the structured Errors array instead.
func formatValidationDetail(ve validator.ValidationErrors) string {
	parts := make([]string, 0, len(ve))
	for _, fe := range ve {
		parts = append(parts, humanizeFieldError(fe))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// humanizeFieldError returns a short sentence for a single validator failure.
// The messages intentionally avoid jargon and always reference the JSON field
// name returned by TagNameFunc.
func humanizeFieldError(fe validator.FieldError) string {
	field := fe.Field()
	param := fe.Param()
	switch fe.Tag() {
	case "required":
		return fmt.Sprintf("%s is required", field)
	case "oneof":
		return fmt.Sprintf("%s must be one of: %s", field, strings.ReplaceAll(param, " ", ", "))
	case "min":
		return fmt.Sprintf("%s must be at least %s", field, param)
	case "max":
		return fmt.Sprintf("%s must be at most %s", field, param)
	case "len":
		return fmt.Sprintf("%s must have length %s", field, param)
	case "email":
		return fmt.Sprintf("%s must be a valid email address", field)
	case "url":
		return fmt.Sprintf("%s must be a valid URL", field)
	case "uuid":
		return fmt.Sprintf("%s must be a valid UUID", field)
	case "gte":
		return fmt.Sprintf("%s must be ≥ %s", field, param)
	case "lte":
		return fmt.Sprintf("%s must be ≤ %s", field, param)
	case "gt":
		return fmt.Sprintf("%s must be > %s", field, param)
	case "lt":
		return fmt.Sprintf("%s must be < %s", field, param)
	default:
		if param != "" {
			return fmt.Sprintf("%s failed %q validation (param=%s)", field, fe.Tag(), param)
		}
		return fmt.Sprintf("%s failed %q validation", field, fe.Tag())
	}
}
