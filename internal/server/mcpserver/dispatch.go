package mcpserver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/stperic/zzrouter/pkg/httperr"
	"github.com/stperic/zzrouter/pkg/observability"

	"github.com/stperic/zzrouter/pkg/utils"
)

const (
	// MaxResponseBytes bounds memory retained from a REST response.
	MaxResponseBytes = 1 << 20
	// errorExcerptBytes keeps unexpected HTTP errors short enough to act on.
	errorExcerptBytes = 512
)

// valueFree retains cancellation and deadlines without authentication values.
type valueFree struct{ context.Context }

func (valueFree) Value(any) any { return nil }

type dispatcher struct {
	cfg   Config
	outer *outerRequest
	// bindings is the registered tool-to-operationId map, read only after registration.
	bindings map[string][]string
}

type routePrefix string

const (
	discoveryRoute routePrefix = "/zzrouter/v1"
	nodesRoute     routePrefix = "/zzrouter/v1/nodes"
	healthRoute    routePrefix = "/zzrouter/v1/health"
	jobsRoute      routePrefix = "/zzrouter/v1/jobs/"
)

type requestPath struct{ value string }

func (p routePrefix) path() requestPath { return requestPath{string(p)} }

// pathSegment admits one identifier, never an encoded path or query.
func pathSegment(prefix routePrefix, id string) (requestPath, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#%") {
		return requestPath{}, newToolError(http.StatusBadRequest, codeInvalidInput, "invalid path identifier: use a nonempty id without /, \\, ?, #, % or dot segments", nil)
	}
	return requestPath{string(prefix) + url.PathEscape(id)}, nil
}

type request struct {
	method, contentType, idempotencyKey, ifMatch string
	path                                         requestPath
	query                                        url.Values
	body                                         []byte
	jsonBody                                     any
	omit                                         []string
}

// problemLine is a Problem as text: its code and detail, each issue, then
// each distinct hint once, since issues often share one remedy.
func problemLine(status int, p utils.ProblemDetails) string {
	line := fmt.Sprintf("HTTP %d %s: %s", status, p.Code, p.Detail)
	var hints []string
	for _, item := range p.Errors {
		line += "\n" + item.Key + ": " + item.Message
		if item.Hint != "" && !slices.Contains(hints, item.Hint) {
			hints = append(hints, item.Hint)
		}
	}
	for _, hint := range hints {
		line += "\n" + hint
	}
	return line
}

func (d dispatcher) call(ctx context.Context, input request, out any) error {
	if input.jsonBody != nil {
		body, err := jsonBody(input.jsonBody, input.omit)
		if err != nil {
			return newToolError(http.StatusInternalServerError, codeInternal, fmt.Sprintf("encode tool input: %v", err), err)
		}
		input.body = body
	}
	req, err := http.NewRequestWithContext(ctx, input.method, input.path.value, bytes.NewReader(input.body))
	if err != nil {
		return newToolError(http.StatusBadRequest, codeInvalidInput, fmt.Sprintf("build REST request: %v", err), err)
	}
	req.URL.RawQuery = input.query.Encode()
	if d.cfg.Streaming(input.method, req.URL.Path) != "" && (d.cfg.SnapshotAllowed == nil || !d.cfg.SnapshotAllowed(req)) {
		return newToolError(http.StatusBadRequest, codeStreamingRefused, fmt.Sprintf("streaming route %s is unavailable to tools; use a snapshot endpoint", req.URL.Path), nil)
	}
	// A fresh cancellable context strips every outer value, including auth markers.
	inner, cancel := context.WithCancel(valueFree{ctx})
	defer cancel()
	stop := context.AfterFunc(d.outer.ctx, cancel) //nolint:contextcheck // Decision 2 bridges HTTP cancellation without inheriting auth values.
	defer stop()
	if d.outer.ctx.Err() != nil {
		cancel()
	}
	req = req.WithContext(context.WithValue(inner, dispatchKey, true)) //nolint:contextcheck // Decision 2 permits only the accounting marker in the value-free context.
	req.RemoteAddr = d.outer.remoteAddr
	observability.InjectTraceContext(d.outer.ctx, req.Header) //nolint:contextcheck // Serialize only the HTTP trace parent into the value-free request.
	for _, name := range append(append([]string(nil), d.cfg.KeyHeaders...), "X-Forwarded-For", "X-Real-IP") {
		for _, value := range d.outer.header.Values(name) {
			req.Header.Add(name, value)
		}
	}
	if input.contentType != "" {
		req.Header.Set("Content-Type", input.contentType)
	}
	if input.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", input.idempotencyKey)
	}
	if input.ifMatch != "" {
		req.Header.Set("If-Match", input.ifMatch)
	}
	req.Header.Set("X-Request-ID", fmt.Sprintf("%s.%d", d.outer.requestID, d.outer.index.Add(1)))
	if err := inner.Err(); err != nil {
		return newToolError(httperr.StatusClientClosedRequest, codeInterrupted, fmt.Sprintf("REST request interrupted: %v", err), err)
	}
	rec := &bufferedResponse{header: make(http.Header), cancel: cancel}
	d.cfg.Engine.ServeHTTP(rec, req)
	// A REST failure is reported as itself, even when its body overflowed or the caller left.
	if rec.statusCode() < http.StatusOK || rec.statusCode() >= http.StatusMultipleChoices {
		mediaType, _, _ := mime.ParseMediaType(rec.header.Get("Content-Type"))
		if mediaType == "application/problem+json" {
			var p utils.ProblemDetails
			if err := json.Unmarshal(rec.body.Bytes(), &p); err == nil && bytes.HasPrefix(bytes.TrimSpace(rec.body.Bytes()), []byte("{")) {
				if p.Status == 0 {
					p.Status = rec.statusCode()
				}
				defaults := utils.NewProblemDetails(p.Status, http.StatusText(p.Status), "", "")
				p.Code = cmp.Or(p.Code, defaults.Code)
				p.Type = cmp.Or(p.Type, defaults.Type)
				p.Title = cmp.Or(p.Title, defaults.Title)
				return &toolError{problem: p, line: problemLine(rec.statusCode(), p)}
			}
		}
		var upstream utils.OpenAIError
		if mediaType == "application/json" && json.Unmarshal(rec.body.Bytes(), &upstream) == nil && upstream.Error.Message != "" {
			code := ""
			if upstream.Error.Code != nil {
				code = *upstream.Error.Code
			}
			failure := newToolError(rec.statusCode(), toolErrorCode(code), upstream.Error.Message, nil)
			failure.line = fmt.Sprintf("HTTP %d %s: %s", rec.statusCode(), failure.problem.Code, upstream.Error.Message)
			return failure
		}
		excerpt := strings.TrimSpace(string(rec.body.Bytes()[:min(rec.body.Len(), errorExcerptBytes)]))
		failure := newToolError(rec.statusCode(), "", fmt.Sprintf("HTTP %d: %s", rec.statusCode(), excerpt), nil)
		failure.problem.Detail = excerpt
		return failure
	}
	if rec.err != nil {
		return rec.err
	}
	if err := inner.Err(); err != nil {
		return newToolError(httperr.StatusClientClosedRequest, codeInterrupted, fmt.Sprintf("REST request interrupted: %v", err), err)
	}
	if rec.statusCode() == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return newToolError(http.StatusBadGateway, codeResponseDecode, fmt.Sprintf("decode REST response: %v", err), err)
	}
	return nil
}

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
	err    error
	cancel context.CancelFunc
}

func (r *bufferedResponse) Header() http.Header { return r.header }
func (*bufferedResponse) Flush()                {}
func (r *bufferedResponse) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
func (r *bufferedResponse) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}
func (r *bufferedResponse) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if len(p) > MaxResponseBytes-r.body.Len() {
		_, _ = r.body.Write(p[:MaxResponseBytes-r.body.Len()])
		r.err = newToolError(http.StatusBadGateway, codeResponseTooLarge, "REST response exceeds the 1 MiB cap; narrow the query and retry", nil)
		r.cancel()
		return 0, r.err
	}
	return r.body.Write(p)
}
