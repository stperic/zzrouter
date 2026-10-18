package mcpserver

import (
	"errors"
	"net/http"

	"github.com/stperic/zzrouter/pkg/utils"
)

// toolErrorCode names a failure the tool layer reports itself, before or after the REST call.
type toolErrorCode string

const (
	codeInvalidInput     toolErrorCode = "mcp_invalid_input"
	codeResponseTooLarge toolErrorCode = "mcp_response_too_large"
	codeStreamingRefused toolErrorCode = "mcp_streaming_refused"
	codeResponseDecode   toolErrorCode = "mcp_response_decode_failed"
	codeInterrupted      toolErrorCode = "mcp_request_interrupted"
	codeInternal         toolErrorCode = "mcp_internal_error"
	codeUpstream         toolErrorCode = "mcp_upstream_error"
)

// toolError is a tool failure: a human line for the text block and an RFC 9457 problem for structuredContent.
type toolError struct {
	problem utils.ProblemDetails
	line    string
	cause   error
	probe   *probeOutput
}

func (e *toolError) Error() string { return e.line }
func (e *toolError) Unwrap() error { return e.cause }

// newToolError builds a synthetic problem; an empty code takes the status slug, as REST problems do.
func newToolError(status int, code toolErrorCode, line string, cause error) *toolError {
	var existing *toolError
	if errors.As(cause, &existing) {
		return &toolError{problem: existing.problem, line: line, cause: cause, probe: existing.probe}
	}
	title := http.StatusText(status)
	if title == "" {
		title = "Upstream Error"
		if code == "" {
			code = codeUpstream
		}
	}
	p := utils.NewProblemDetails(status, title, line, "")
	if code != "" {
		p.Code = string(code)
	}
	return &toolError{problem: *p, line: line, cause: cause}
}

// StructuredContent supplies the product's problem and optional probe evidence.
func (e *toolError) StructuredContent() any {
	if e.probe != nil {
		return struct {
			utils.ProblemDetails
			*probeOutput
		}{e.problem, e.probe}
	}
	return e.problem
}
