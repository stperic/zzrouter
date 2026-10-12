package mcpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// toolErrorMiddleware copies a toolError's problem into structuredContent; it changes nothing else.
func toolErrorMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if method == "tools/call" {
			if call, ok := result.(*mcp.CallToolResult); ok && call != nil && call.IsError {
				var failure *toolError
				if errors.As(call.GetError(), &failure) {
					call.StructuredContent = failure.problem
					if failure.probe != nil {
						call.StructuredContent = struct {
							utils.ProblemDetails
							*probeOutput
						}{failure.problem, failure.probe}
					}
				}
			}
		}
		return result, err
	}
}
